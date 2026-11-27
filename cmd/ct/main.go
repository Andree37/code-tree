// ct — code-tree: a dependency graph of work threads shared by humans and agents.
//
// Every node is a markdown file in .tree/nodes/<id>.md with a small frontmatter.
// A node "blocks" one or more parents; a parent waits until all of its children
// are resolved. Nodes with nothing above them are HEADs (goals).
//
// Everything an agent needs is plain text:
//
//	.tree/GRAPH.md      regenerated on every change (mermaid + tables)
//	.tree/PROTOCOL.md   how agents should work with the tree
//	ct show / next / ls plain text that agents read directly
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- args

// Args holds positionals and flags; flags may appear anywhere, as --x v, --x=v or -x v.
type Args struct {
	Pos   []string
	flags map[string][]string
}

type flagSpec struct {
	name  string
	short string
	value bool // takes a value
	help  string
}

func (a *Args) Get(name, def string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}
func (a *Args) All(name string) []string { return a.flags[name] }
func (a *Args) Bool(name string) bool {
	v := strings.ToLower(a.Get(name, ""))
	return v == "true" || v == "1" || v == "yes"
}
func (a *Args) Has(name string) bool { return len(a.flags[name]) > 0 }

func parseArgs(cmd string, argv []string, specs []flagSpec) *Args {
	a := &Args{flags: map[string][]string{}}
	find := func(tok string) *flagSpec {
		for k := range specs {
			if tok == "--"+specs[k].name || (specs[k].short != "" && tok == "-"+specs[k].short) {
				return &specs[k]
			}
		}
		return nil
	}
	for k := 0; k < len(argv); k++ {
		tok := argv[k]
		if tok == "--" {
			a.Pos = append(a.Pos, argv[k+1:]...)
			break
		}
		if tok == "-h" || tok == "--help" {
			usageCmd(cmd)
			os.Exit(0)
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			a.Pos = append(a.Pos, tok)
			continue
		}
		name, val, hasVal := strings.Cut(tok, "=")
		s := find(name)
		if s == nil {
			die("%s: unknown flag %s (try `ct %s --help`)", cmd, name, cmd)
		}
		if !s.value {
			if !hasVal {
				val = "true"
			}
			a.flags[s.name] = append(a.flags[s.name], val) // --force=false means false
			continue
		}
		if !hasVal {
			if k+1 >= len(argv) {
				die("%s: %s needs a value", cmd, name)
			}
			k++
			val = argv[k]
		}
		a.flags[s.name] = append(a.flags[s.name], val)
	}
	return a
}

// ---------------------------------------------------------------- commands

type command struct {
	name, args, help string
	flags            []flagSpec
	run              func(*Args)
}

var (
	fBy      = flagSpec{"by", "", true, "who is acting (default: $CT_AGENT or 'human')"}
	fForce   = flagSpec{"force", "", false, "skip safety checks"}
	fOutcome = flagSpec{"outcome", "o", true, "one-line result other nodes will see"}
	fNote    = flagSpec{"note", "", true, "log message"}
	fNoOpen  = flagSpec{"no-open", "", false, "do not open a browser"}
	fTouches = flagSpec{"touches", "t", true, "code paths/patterns this node owns (repeatable, comma ok; 'auto' = changed since claim)"}
)

var commands []command

// lockedCommands change the tree, so they hold its lock from load to save.
// Long-running views (watch, see, serve), edit and chat (whose agent runs ct
// itself) do not.
var lockedCommands = map[string]bool{
	"add": true, "claim": true, "ask": true, "ready": true, "done": true, "drop": true, "open": true,
	"set": true, "accept": true, "reject": true, "merge": true, "check": true, "answer": true,
	"decide": true, "talk": true, "work": true, "run": true, "note": true, "link": true, "unlink": true,
	"rm": true, "touch": true, "sync": true, "ack": true, "fmt": true,
}

func init() {
	commands = []command{
		{"init", "", "create .tree/ in the current directory", []flagSpec{{"code", "", true, "path of the code repo (default: this directory)"}}, cmdInit},
		{"add", "TITLE", "add a node (a HEAD if it blocks nothing)", []flagSpec{
			{"blocks", "b", true, "node that waits on this one (repeatable)"},
			{"desc", "d", true, "context: what and why"},
			{"mode", "m", true, "discuss | research | build | review | fix (free text, default build)"},
			{"agent", "a", true, "assigned agent or person"},
			{"status", "s", true, "initial status (default open)"},
			fTouches,
			{"check", "", true, "shell command that must pass before ready/done (runs in the code repo)"},
			fBy,
		}, cmdAdd},
		{"ls", "[HEAD]", "print the tree(s)", []flagSpec{{"all", "", false, "include resolved nodes and heads"}}, cmdLs},
		{"show", "ID", "one node with its chain to the head and children's outcomes", nil, cmdShow},
		{"next", "", "actionable nodes, highest leverage first", []flagSpec{
			{"for", "", true, "agent | human | all (default all)"},
			{"head", "", true, "only under this head"},
			{"n", "n", true, "limit"},
		}, cmdNext},
		{"claim", "ID", "mark a node working", []flagSpec{fBy, fForce}, cmdClaim},
		{"ask", "ID QUESTION", "mark a node needs-you with a question for the human", []flagSpec{fBy}, cmdAsk},
		{"ready", "ID", "mark a node ready for review", statusFlags(), statusCmd("ready")},
		{"done", "ID", "mark a node done", statusFlags(), statusCmd("done")},
		{"drop", "ID", "mark a node dropped", statusFlags(), statusCmd("dropped")},
		{"open", "ID", "mark a node open", statusFlags(), statusCmd("open")},
		{"set", "ID STATUS", "set any status: " + strings.Join(statuses, " | "), statusFlags(), cmdSet},
		{"accept", "ID", "accept a proposed node (it becomes open)", []flagSpec{fNote, fBy}, cmdAccept},
		{"reject", "ID [REASON]", "reject a proposed node (it becomes dropped)", []flagSpec{fBy}, cmdReject},
		{"merge", "ID INTO", "fold a node into another one and drop it", []flagSpec{fBy}, cmdMerge},
		{"check", "ID", "run a node's check now (or set it)", []flagSpec{
			{"set", "", true, "set the check command"},
			{"clear", "", false, "remove the check"},
			fBy,
		}, cmdCheck},
		{"answer", "ID TEXT", "answer a needs-you node; the answer becomes a decision for it and everything below", []flagSpec{fBy}, cmdAnswer},
		{"decide", "ID TEXT", "record a decision that holds for a node and everything below it", []flagSpec{fBy}, cmdDecide},
		{"chat", "ID [MESSAGE]", "talk to the chat agent about a node (no message: print the chat)", []flagSpec{fBy}, cmdChat},
		{"talk", "ID TEXT", "message the agent on a node (its owner, else the agent in this herdr tab) via intercom", []flagSpec{{"to", "", true, "recipient: intercom name, tab, pane:<id>"}, fBy}, cmdTalk},
		{"work", "ID", "ask the agent in this tab (or --to) to work on a node now", []flagSpec{{"to", "", true, "recipient: intercom name, tab, pane:<id>"}, fBy}, cmdWork},
		{"see", "", "the conversation graph of the package Claude is working in (follows the Claude in this herdr tab)", []flagSpec{{"session", "", true, "follow this session (id, prefix or intercom name)"}}, cmdSee},
		{"sessions", "", "Claude sessions with a conversation tree", nil, cmdSessions},
		{"where", "SESSION...", "per Claude session: its package, current card and how many points wait for you / are open (tab-separated, for herdr's sidebar)", nil, cmdWhere},
		{"recheck", "", "re-judge every box waiting for you; settle the ones that were only offers", nil, cmdRecheck},
		{"install", "claude", "add ct's hooks to ~/.claude/settings.json so sessions build their trees", nil, cmdInstall},
		{"hook", "EVENT", "Claude Code hook entry (prompt, stop, end)", nil, cmdHook},
		{"turn", "", "place a session's new turns in its package trees (run by the stop hook)", []flagSpec{
			{"session", "", true, "session id"}, {"transcript", "", true, "transcript path"}, {"cwd", "", true, "session folder"}}, cmdTurn},
		{"run", "ID", "start a new agent on an open node in its own herdr tab", []flagSpec{{"agent", "", true, "agent command (default claude)"}, fForce, fBy}, cmdRun},
		{"watch", "", "live view of the tree for a pane beside your agent (single-key actions)", nil, cmdWatch},
		{"note", "ID TEXT", "append to a node's log", []flagSpec{fBy}, cmdNote},
		{"link", "CHILD PARENT", "make CHILD block PARENT", []flagSpec{fBy}, cmdLink},
		{"unlink", "CHILD PARENT", "remove CHILD → PARENT", []flagSpec{fBy}, cmdUnlink},
		{"rm", "ID", "delete a node file", []flagSpec{fForce}, cmdRm},
		{"edit", "ID", "open a node in $EDITOR", nil, cmdEdit},
		{"graph", "", "dump the graph (md | mermaid)", []flagSpec{
			{"format", "f", true, "md | mermaid (default md)"},
		}, cmdGraph},
		{"view", "", "write .tree/graph.html and open it", []flagSpec{fNoOpen}, cmdView},
		{"serve", "", "live graph view in the browser", []flagSpec{{"port", "p", true, "port (default 7337)"}, fNoOpen}, cmdServe},
		{"touch", "ID PATH...", "set code paths a node owns ('dir/', globs, 'auto')", []flagSpec{{"rm", "", false, "remove these paths instead"}}, cmdTouch},
		{"sync", "", "catch up with code changes since the last sync (read-only git)", []flagSpec{
			{"check", "", false, "exit 1 if anything is stale or unmapped (for CI)"},
			{"dry-run", "", false, "report without writing"},
			{"run-checks", "", false, "re-run checks of stale nodes: pass clears stale, fail reopens"},
		}, cmdSync},
		{"ack", "ID", "mark a stale node as still valid after code changes", []flagSpec{fNote, fOutcome, fBy}, cmdAck},
		{"fmt", "", "rewrite every node file in the current format", nil, cmdFmt},
		{"prompt", "", "print the agent protocol", nil, func(*Args) { fmt.Print(protocol) }},
	}
}

func statusFlags() []flagSpec {
	return []flagSpec{fOutcome, fNote, fTouches, fForce, fBy, {"skip-check", "", false, "do not run the node's check (logged)"}}
}

func usage() {
	fmt.Print("ct — code-tree: dependency graph of work for humans and agents\n\nusage: ct <command> [args]\n\n")
	for _, c := range commands {
		fmt.Printf("  %-8s %-14s %s\n", c.name, c.args, c.help)
	}
	fmt.Println("\n`ct <command> --help` for flags. Node ids match by unique prefix or substring.")
}

func usageCmd(name string) {
	for _, c := range commands {
		if c.name == name {
			fmt.Printf("usage: ct %s %s\n\n%s\n", c.name, c.args, c.help)
			for _, f := range c.flags {
				flag := "--" + f.name
				if f.short != "" {
					flag = "-" + f.short + ", " + flag
				}
				if f.value {
					flag += " V"
				}
				fmt.Printf("  %-18s %s\n", flag, f.help)
			}
		}
	}
}

func need(a *Args, n int, cmd string) {
	if len(a.Pos) < n {
		usageCmd(cmd)
		os.Exit(2)
	}
}

func who(a *Args) string {
	// an agent may name itself with --by, but never claim to be the human
	if by := plainValue("--by", strings.TrimSpace(a.Get("by", ""))); by != "" && !(inAgent() && strings.EqualFold(by, "human")) {
		return by
	}
	if env := plainValue("CT_AGENT", strings.TrimSpace(os.Getenv("CT_AGENT"))); env != "" {
		return env
	}
	if me := flat(icMe()); me != "" { // this agent session's intercom name
		return me
	}
	if os.Getenv("CLAUDECODE") == "1" {
		return "claude"
	}
	return "human"
}

// inAgent is true when ct runs on behalf of an agent rather than the human.
func inAgent() bool {
	return os.Getenv("CT_AGENT") != "" || os.Getenv("CLAUDECODE") == "1" || os.Getenv("IC_ID") != ""
}

// humanOnly stops agents from doing what only the human may do.
func humanOnly(a *Args, what string) {
	if who(a) != "human" {
		die("only the human can %s; ask them with `ct ask`", what)
	}
}

func appendLog(n *Node, text, by string) {
	body := strings.TrimRight(n.Body, "\n")
	if !strings.Contains(body, "## Log") {
		body += "\n\n## Log"
	}
	n.Body = body + fmt.Sprintf("\n- %s [%s] %s\n", now(), strings.ReplaceAll(flat(by), "]", ")"), flat(text))
}

// oneLine makes free text fit one line: newlines and other control
// characters become spaces.
func oneLine(s string) string { return flat(s) }

func cmdInit(a *Args) {
	cwd, _ := os.Getwd()
	root := filepath.Join(cwd, treeDir)
	if err := os.MkdirAll(filepath.Join(root, "nodes"), 0o755); err != nil {
		die("%v", err)
	}
	if old, err := os.ReadFile(filepath.Join(root, "config")); err != nil || a.Has("code") {
		cfg := defaultConfig
		if err == nil {
			cfg = string(old) // keep the existing config; only the code line changes
		}
		if c := a.Get("code", ""); c != "" {
			cfg = setConfigLine(cfg, "code", c)
		}
		if err := os.WriteFile(filepath.Join(root, "config"), []byte(cfg), 0o644); err != nil {
			die("%v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "PROTOCOL.md"), []byte(protocol), 0o644); err != nil {
		die("%v", err)
	}
	writeGraphMD(root)
	fmt.Println("initialized", root)
	fmt.Println("Tell your agents about it, e.g. add to CLAUDE.md / AGENTS.md:")
	fmt.Println("  > Work is tracked in .tree/ — read .tree/PROTOCOL.md and use the `ct` CLI.")
}

func cmdAdd(a *Args) {
	need(a, 1, "add")
	g := load(requireTree())
	title := oneLine(strings.Join(a.Pos, " "))
	var parents []string
	for _, b := range a.All("blocks") {
		if p := g.resolve(b); !slices.Contains(parents, p) {
			parents = append(parents, p)
		}
	}
	status := a.Get("status", "open")
	if !slices.Contains(statuses, status) {
		die("status must be one of %s", strings.Join(statuses, ", "))
	}
	if who(a) != "human" && loadConfig(g.Dir).Approve != "none" {
		status = "proposed" // agents propose; a human accepts (see `ct accept`)
	}
	base := slugify(title)
	id := base
	for k := 2; g.Nodes[id] != nil || nodeFileExists(g.Dir, id); k++ {
		id = fmt.Sprintf("%s-%d", base, k)
	}
	body := "\n## Context\n"
	if d := strings.TrimSpace(a.Get("desc", "")); d != "" {
		body += d + "\n"
	}
	body += "\n## Log\n"
	n := &Node{
		ID: id, Title: title, Mode: plainValue("--mode", a.Get("mode", "build")), Agent: plainValue("--agent", a.Get("agent", "")), Status: status,
		Blocks: append([]string{}, parents...), Created: now(), Extra: map[string]string{}, Body: body,
	}
	if n.Blocks == nil {
		n.Blocks = []string{}
	}
	recordCodeTouches(g, n, a.All("touches"))
	if a.Has("check") {
		humanOnly(a, "set a check (it runs as a shell command)")
	}
	n.Check = oneLine(a.Get("check", ""))
	appendLog(n, "created", who(a))
	changed := []*Node{n}
	g.Nodes[id] = n
	for _, p := range parents { // a new child reopens a resolved/ready parent (a proposal only once accepted)
		changed = append(changed, reopenFor(g, p, id, "new", who(a))...)
	}
	g.mutate(changed...)
	fmt.Println(id)
}

// statusGuards refuses a status change the rules do not allow. It runs again
// after a slow check, against what is on disk then.
func statusGuards(a *Args, g *Graph, i, status string) {
	n, f := g.Nodes[i], g.Info[i]
	by := who(a)
	if n.Status == "proposed" && status != "proposed" && by != "human" {
		die("%s is only proposed; a human has to `ct accept` (or `ct reject`) it first", i)
	}
	if status == "working" {
		if n.Status == "dropped" {
			die("%s was dropped; a human has to reopen it (`ct open %s`) before it can be claimed", i, i)
		}
		if n.Status == "working" && n.Agent != "" && n.Agent != by && !a.Bool("force") {
			die("%s is already claimed by %s (use --force to take it)", i, n.Agent)
		}
		if f.State == "waiting" && !a.Bool("force") {
			die("%s is %s; pick something from `ct next`", i, f.Label)
		}
	}
	if (status == "done" || status == "ready") && !a.Bool("force") {
		open := 0 // count children directly: a proposed or waiting-for-you label hides them
		for _, c := range f.Children {
			if !resolved(g.Nodes[c].Status) {
				open++
			}
		}
		if open > 0 {
			die("%s has %d unresolved point(s) under it; resolve them first (or --force)", i, open)
		}
		if n.Mode == "discuss" && a.Get("outcome", n.Outcome) == "" {
			die("%s is a discuss node: close it with the decision, --outcome \"...\"", i)
		}
	}
}

func setStatus(a *Args, status, notePrefix string) {
	need(a, 1, "set")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if a.Bool("skip-check") {
		humanOnly(a, "skip a check")
	}
	statusGuards(a, g, i, status)
	checkNote := ""
	if (status == "done" || status == "ready") && n.Check != "" {
		if a.Bool("skip-check") {
			checkNote = " (check skipped)"
		} else {
			before, check := n.Status, n.Check
			ok, out := runCheck(g, n)
			// a check can take minutes; anything written meanwhile (a decision,
			// a note) must survive, so carry on from what is on disk now
			g = load(g.Dir)
			if n = g.Nodes[i]; n == nil {
				die("%s was removed while its check ran", i)
			}
			if n.Status != before || n.Check != check {
				die("%s changed while its check ran (now %s); run it again if it still applies", i, label(n.Status))
			}
			statusGuards(a, g, i, status) // e.g. a point added under it meanwhile
			if !ok {
				appendLog(n, "check failed, stays "+n.Status+": "+oneLine(lastLines(out, 3)), who(a))
				g.mutate(n)
				die("check failed for %s: %s\n%s", i, n.Check, lastLines(out, 15))
			}
			checkNote = " (check passed)"
		}
	}
	old := n.Status
	n.Status = status
	if status == "working" {
		if old != "working" || n.Base == "" { // re-claiming keeps the original starting point
			n.Base = codeHead(loadConfig(g.Dir)) // "auto" touches diff from here
		}
		n.Agent = who(a)
	}
	if !resolved(status) {
		n.Stale = nil // reopened work is current again
	}
	recordCode(g, n, a.All("touches"))
	if o := a.Get("outcome", ""); o != "" {
		n.Outcome = oneLine(o)
	}
	msg := old + " → " + status + checkNote
	text := a.Get("note", a.Get("outcome", ""))
	if text != "" {
		msg += ": " + notePrefix + text
	}
	appendLog(n, msg, who(a))
	g.mutate(n)
	fmt.Printf("%s: %s\n", i, label(status))
	if status != "working" {
		what := "set it to " + label(status)
		if old == "ready" && status == "open" || old == "done" && status == "open" {
			what = "sent it back to Open"
		}
		if text != "" {
			what += ": " + text
		}
		tell(g, n, who(a), what, status == "open")
	}
}

func statusCmd(status string) func(*Args) {
	return func(a *Args) { setStatus(a, status, "") }
}

func cmdSet(a *Args) {
	need(a, 2, "set")
	if !slices.Contains(statuses, a.Pos[1]) {
		die("status must be one of %s", strings.Join(statuses, ", "))
	}
	setStatus(a, a.Pos[1], "")
}

func cmdClaim(a *Args) {
	need(a, 1, "claim")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	if g.Nodes[i].Status == "proposed" { // even the human accepts first, so the step is recorded
		die("%s is only proposed; a human has to `ct accept` it first", i)
	}
	a.Pos[0] = i
	setStatus(a, "working", "") // checks who holds it, waiting and dropped
	warnOverlap(load(g.Dir), i, who(a))
}

func cmdAsk(a *Args) {
	need(a, 2, "ask")
	a.flags["note"] = []string{strings.Join(a.Pos[1:], " ")}
	setStatus(a, "needs-you", "QUESTION: ")
}

func cmdNote(a *Args) {
	need(a, 2, "note")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	appendLog(g.Nodes[i], strings.Join(a.Pos[1:], " "), who(a))
	g.mutate(g.Nodes[i])
	tell(g, g.Nodes[i], who(a), "noted: "+strings.Join(a.Pos[1:], " "), false)
}

func cmdLink(a *Args) {
	need(a, 2, "link")
	g := load(requireTree())
	c, p := g.resolve(a.Pos[0]), g.resolve(a.Pos[1])
	if c == p || g.descendants(c)[p] {
		die("linking %s → %s would create a cycle", c, p)
	}
	n := g.Nodes[c]
	changed := []*Node{n}
	if !slices.Contains(n.Blocks, p) {
		n.Blocks = append(n.Blocks, p)
		appendLog(n, fmt.Sprintf("now blocks `%s`", p), who(a))
		changed = append(changed, reopenFor(g, p, c, "linked", who(a))...)
	}
	g.mutate(changed...)
}

// reopenFor reopens a finished parent that now waits on open work, so a
// Done node never sits over unresolved points.
func reopenFor(g *Graph, parent, child, why, by string) []*Node {
	pn, cn := g.Nodes[parent], g.Nodes[child]
	if pn == nil || cn == nil || resolved(cn.Status) || cn.Status == "proposed" {
		return nil
	}
	if pn.Status != "done" && pn.Status != "ready" {
		return nil
	}
	pn.Status = "open"
	appendLog(pn, fmt.Sprintf("reopened: %s prerequisite `%s`", why, child), by)
	out := []*Node{pn}
	for _, gp := range pn.Blocks { // and so on up: what it blocks is not finished either
		out = append(out, reopenFor(g, gp, parent, "reopened", by)...)
	}
	return out
}

func cmdUnlink(a *Args) {
	need(a, 2, "unlink")
	g := load(requireTree())
	c, p := g.resolve(a.Pos[0]), g.resolve(a.Pos[1])
	n := g.Nodes[c]
	if k := slices.Index(n.Blocks, p); k >= 0 {
		n.Blocks = slices.Delete(n.Blocks, k, k+1)
		appendLog(n, fmt.Sprintf("no longer blocks `%s`", p), who(a))
	}
	g.mutate(n)
}

func cmdRm(a *Args) {
	need(a, 1, "rm")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	if kids := g.Info[i].Children; len(kids) > 0 && !a.Bool("force") {
		die("%s has children (%s); use --force or `ct drop`", i, strings.Join(kids, ", "))
	}
	var changed []*Node
	for _, id := range g.IDs {
		n := g.Nodes[id]
		if k := slices.Index(n.Blocks, i); k >= 0 {
			n.Blocks = slices.Delete(n.Blocks, k, k+1)
			changed = append(changed, n)
		}
	}
	if err := os.Remove(filepath.Join(g.Dir, "nodes", i+".md")); err != nil {
		die("%v", err)
	}
	// its chat goes too, or a new node with the same id would inherit it
	_ = os.Remove(chatPath(g.Dir, i))
	_ = os.Remove(pendingPath(g.Dir, i))
	g.mutate(changed...)
	fmt.Println("removed", i)
}

func cmdEdit(a *Args) {
	need(a, 1, "edit")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	parts := strings.Fields(os.Getenv("EDITOR"))
	if len(parts) == 0 { // unset or blank
		parts = []string{"vi"}
	}
	cmd := exec.Command(parts[0], append(parts[1:], filepath.Join(g.Dir, "nodes", i+".md"))...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
	writeGraphMD(g.Dir)
}

func cmdLs(a *Args) {
	g := load(requireTree())
	var heads []string
	switch {
	case len(a.Pos) > 0:
		heads = []string{g.resolve(a.Pos[0])}
	default:
		for _, h := range g.heads() {
			if a.Bool("all") || !resolved(g.Nodes[h].Status) {
				heads = append(heads, h)
			}
		}
	}
	if len(heads) == 0 {
		fmt.Println(`no open heads. ` + "`ct add \"<goal>\"`" + ` creates one.`)
	}
	for _, h := range heads {
		g.printTree(h, a.Bool("all"))
	}
}

func cmdShow(a *Args) {
	need(a, 1, "show")
	g := load(requireTree())
	fmt.Print(g.showText(g.resolve(a.Pos[0])))
}

func (g *Graph) showText(i string) string {
	var b strings.Builder
	n, f := g.Nodes[i], g.Info[i]
	var chain []string // follow first parent up to a head for the "why" chain
	for cur, seen := f.Parents, map[string]bool{i: true}; len(cur) > 0 && !seen[cur[0]]; cur = g.Info[cur[0]].Parents {
		seen[cur[0]] = true // a loop in old data must not hang ct show
		chain = append(chain, cur[0])
	}
	fmt.Fprintln(&b, n.Title)
	fmt.Fprintf(&b, "  id: %s   state: %s   kind: %s   unblocks: %d\n", i, f.Label, f.Kind, f.Unblocks)
	if n.Mode != "" || n.Agent != "" {
		fmt.Fprintf(&b, "  mode: %s   agent: %s\n", dash(n.Mode), dash(n.Agent))
	}
	if n.Outcome != "" {
		fmt.Fprintln(&b, "  outcome:", n.Outcome)
	}
	if n.Check != "" {
		fmt.Fprintln(&b, "  check:", n.Check)
	}
	if len(n.Touches) > 0 {
		fmt.Fprintln(&b, "  touches:", strings.Join(n.Touches, ", "))
	}
	if n.At != "" {
		fmt.Fprintln(&b, "  resolved at:", short(n.At))
	}
	if len(n.Stale) > 0 {
		fmt.Fprintln(&b, "  STALE, code changed since:", strings.Join(n.Stale, ", "))
	}
	if ds := g.decisionsInForce(i); len(ds) > 0 {
		fmt.Fprintln(&b, "\nDecisions in force (follow these):")
		for _, d := range ds {
			from := ""
			if d[0] != i {
				from = "  (from " + d[0] + ")"
			}
			fmt.Fprintf(&b, "  - %s%s\n", d[1], from)
		}
	}
	if len(chain) > 0 {
		fmt.Fprintln(&b, "\nWhy (chain to head):")
		for d, p := range chain {
			fmt.Fprintf(&b, "  %s↑ %s\n", strings.Repeat("  ", d), g.line(p))
		}
		if len(f.Parents) > 1 {
			fmt.Fprintln(&b, "  also blocks:", strings.Join(f.Parents[1:], ", "))
		}
	}
	if len(f.Children) > 0 {
		fmt.Fprintln(&b, "\nWaits on:")
		for _, c := range f.Children {
			fmt.Fprintln(&b, "  -", g.line(c))
			if o := g.Nodes[c].Outcome; o != "" {
				fmt.Fprintln(&b, "      ⇒", o)
			}
		}
	}
	fmt.Fprintln(&b, "\n"+strings.TrimSpace(n.Body))
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func cmdNext(a *Args) {
	g := load(requireTree())
	forWho := a.Get("for", "all")
	if !slices.Contains([]string{"agent", "human", "all"}, forWho) {
		die("--for must be agent, human or all")
	}
	ids := g.actionable(forWho)
	if h := a.Get("head", ""); h != "" {
		h = g.resolve(h)
		ids = slices.DeleteFunc(ids, func(i string) bool { return !slices.Contains(g.headsOf(i), h) })
	}
	if s := a.Get("n", ""); s != "" {
		k, err := strconv.Atoi(s)
		if err != nil || k < 0 {
			die("-n must be a number, 0 or more")
		}
		if k < len(ids) {
			ids = ids[:k]
		}
	}
	if len(ids) == 0 {
		fmt.Println("nothing actionable.")
	}
	for _, i := range ids {
		fmt.Println(g.line(i))
	}
}

func cmdGraph(a *Args) {
	dir := requireTree()
	format := a.Get("format", "md")
	switch format {
	case "mermaid":
		g := load(dir)
		fmt.Println(g.mermaid(g.IDs))
	case "md":
		writeGraphMD(dir)
		data, _ := os.ReadFile(filepath.Join(dir, "GRAPH.md"))
		os.Stdout.Write(data)
	default:
		die("--format must be md or mermaid")
	}
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	name := os.Args[1]
	for _, c := range commands {
		if c.name == name {
			lockWanted = lockedCommands[name]
			c.run(parseArgs(name, os.Args[2:], c.flags))
			return
		}
	}
	die("unknown command '%s' (see `ct --help`)", name)
}

func cmdFmt(*Args) {
	g := load(requireTree())
	for _, i := range g.IDs {
		p := filepath.Join(g.Dir, "nodes", i+".md")
		if err := writeAtomic(p, []byte(dumpNode(g.Nodes[i]))); err != nil {
			die("%v", err)
		}
	}
	writeGraphMD(g.Dir)
	fmt.Printf("rewrote %d node(s)\n", len(g.IDs))
}

// setConfigLine replaces "key = ..." in a config file, or adds it.
func setConfigLine(cfg, key, value string) string {
	lines := strings.Split(cfg, "\n")
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = key + " = " + value
			return strings.Join(lines, "\n")
		}
	}
	return strings.TrimRight(cfg, "\n") + "\n" + key + " = " + value + "\n"
}
