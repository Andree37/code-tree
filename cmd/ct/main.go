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
//	ct ... --json       machine-readable output for every read command
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
func (a *Args) Bool(name string) bool    { return len(a.flags[name]) > 0 }
func (a *Args) Has(name string) bool     { return len(a.flags[name]) > 0 }

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
			a.flags[s.name] = append(a.flags[s.name], "true")
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
	fJSON    = flagSpec{"json", "", false, "machine-readable output"}
	fForce   = flagSpec{"force", "", false, "skip safety checks"}
	fOutcome = flagSpec{"outcome", "o", true, "one-line result other nodes will see"}
	fNote    = flagSpec{"note", "", true, "log message"}
	fNoOpen  = flagSpec{"no-open", "", false, "do not open a browser"}
	fTouches = flagSpec{"touches", "t", true, "code paths/patterns this node owns (repeatable, comma ok; 'auto' = changed since claim)"}
)

var commands []command

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
			fBy,
		}, cmdAdd},
		{"ls", "[HEAD]", "print the tree(s)", []flagSpec{{"all", "", false, "include resolved nodes and heads"}, fJSON}, cmdLs},
		{"show", "ID", "one node with its chain to the head and children's outcomes", []flagSpec{fJSON}, cmdShow},
		{"next", "", "actionable nodes, highest leverage first", []flagSpec{
			{"for", "", true, "agent | human | all (default all)"},
			{"head", "", true, "only under this head"},
			{"n", "n", true, "limit"},
			fJSON,
		}, cmdNext},
		{"claim", "ID", "mark a node working", []flagSpec{fBy, fForce}, cmdClaim},
		{"ask", "ID QUESTION", "mark a node needs-you with a question for the human", []flagSpec{fBy}, cmdAsk},
		{"ready", "ID", "mark a node ready for review", statusFlags(), statusCmd("ready")},
		{"done", "ID", "mark a node done", statusFlags(), statusCmd("done")},
		{"drop", "ID", "mark a node dropped", statusFlags(), statusCmd("dropped")},
		{"open", "ID", "mark a node open", statusFlags(), statusCmd("open")},
		{"set", "ID STATUS", "set any status: " + strings.Join(statuses, " | "), statusFlags(), cmdSet},
		{"note", "ID TEXT", "append to a node's log", []flagSpec{fBy}, cmdNote},
		{"link", "CHILD PARENT", "make CHILD block PARENT", []flagSpec{fBy}, cmdLink},
		{"unlink", "CHILD PARENT", "remove CHILD → PARENT", []flagSpec{fBy}, cmdUnlink},
		{"rm", "ID", "delete a node file", []flagSpec{fForce}, cmdRm},
		{"edit", "ID", "open a node in $EDITOR", nil, cmdEdit},
		{"graph", "", "dump the graph (md | json | mermaid)", []flagSpec{
			{"format", "f", true, "md | json | mermaid (default md)"},
			fJSON,
			{"full", "", false, "json: include node bodies"},
		}, cmdGraph},
		{"view", "", "write .tree/graph.html and open it", []flagSpec{fNoOpen}, cmdView},
		{"serve", "", "live graph view in the browser", []flagSpec{{"port", "p", true, "port (default 7337)"}, fNoOpen}, cmdServe},
		{"touch", "ID PATH...", "set code paths a node owns ('dir/', globs, 'auto')", []flagSpec{{"rm", "", false, "remove these paths instead"}}, cmdTouch},
		{"sync", "", "catch up with code changes since the last sync (read-only git)", []flagSpec{
			{"check", "", false, "exit 1 if anything is stale or unmapped (for CI)"},
			{"dry-run", "", false, "report without writing"},
		}, cmdSync},
		{"ack", "ID", "mark a stale node as still valid after code changes", []flagSpec{fNote, fOutcome, fBy}, cmdAck},
		{"prompt", "", "print the agent protocol", nil, func(*Args) { fmt.Print(protocol) }},
	}
}

func statusFlags() []flagSpec { return []flagSpec{fOutcome, fNote, fTouches, fForce, fBy} }

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
	if by := a.Get("by", ""); by != "" {
		return by
	}
	if env := os.Getenv("CT_AGENT"); env != "" {
		return env
	}
	return "human"
}

func appendLog(n *Node, text, by string) {
	body := strings.TrimRight(n.Body, "\n")
	if !strings.Contains(body, "## Log") {
		body += "\n\n## Log"
	}
	n.Body = body + fmt.Sprintf("\n- %s [%s] %s\n", now(), by, text)
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func cmdInit(a *Args) {
	cwd, _ := os.Getwd()
	root := filepath.Join(cwd, treeDir)
	if err := os.MkdirAll(filepath.Join(root, "nodes"), 0o755); err != nil {
		die("%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "config")); err != nil || a.Has("code") {
		cfg := defaultConfig
		if c := a.Get("code", ""); c != "" {
			cfg = strings.Replace(cfg, "code = .", "code = "+c, 1)
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
	base := slugify(title)
	id := base
	for k := 2; g.Nodes[id] != nil; k++ {
		id = fmt.Sprintf("%s-%d", base, k)
	}
	body := "\n## Context\n"
	if d := strings.TrimSpace(a.Get("desc", "")); d != "" {
		body += d + "\n"
	}
	body += "\n## Log\n"
	n := &Node{
		ID: id, Title: title, Mode: a.Get("mode", "build"), Agent: a.Get("agent", ""), Status: status,
		Blocks: append([]string{}, parents...), Created: now(), Extra: map[string]string{}, Body: body,
	}
	if n.Blocks == nil {
		n.Blocks = []string{}
	}
	recordCodeTouches(g, n, a.All("touches"))
	appendLog(n, "created", who(a))
	changed := []*Node{n}
	for _, p := range parents { // a new child reopens a resolved/ready parent
		if pn := g.Nodes[p]; pn.Status == "done" || pn.Status == "ready" {
			pn.Status = "open"
			appendLog(pn, fmt.Sprintf("reopened: new prerequisite `%s`", id), who(a))
			changed = append(changed, pn)
		}
	}
	g.mutate(changed...)
	fmt.Println(id)
}

func setStatus(a *Args, status, notePrefix string) {
	need(a, 1, "set")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n, f := g.Nodes[i], g.Info[i]
	if (status == "done" || status == "ready") && f.State == "waiting" && !a.Bool("force") {
		die("%s is %s; resolve its children first (or --force)", i, f.Label)
	}
	old := n.Status
	n.Status = status
	if status == "working" {
		if a.Has("by") {
			n.Agent = a.Get("by", "")
		}
		n.Base = codeHead(loadConfig(g.Dir)) // "auto" touches diff from here
	}
	recordCode(g, n, a.All("touches"))
	if o := a.Get("outcome", ""); o != "" {
		n.Outcome = oneLine(o)
	}
	msg := old + " → " + status
	text := a.Get("note", a.Get("outcome", ""))
	if text != "" {
		msg += ": " + notePrefix + text
	}
	appendLog(n, msg, who(a))
	g.mutate(n)
	fmt.Printf("%s: %s\n", i, label(status))
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
	f, n := g.Info[i], g.Nodes[i]
	by := a.Get("by", "")
	if f.State == "working" && n.Agent != "" && n.Agent != by && !a.Bool("force") {
		die("%s is already claimed by %s (use --force to take it)", i, n.Agent)
	}
	if f.State == "waiting" && !a.Bool("force") {
		die("%s is %s; pick something from `ct next`", i, f.Label)
	}
	setStatus(a, "working", "")
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
}

func cmdLink(a *Args) {
	need(a, 2, "link")
	g := load(requireTree())
	c, p := g.resolve(a.Pos[0]), g.resolve(a.Pos[1])
	if c == p || g.descendants(c)[p] {
		die("linking %s → %s would create a cycle", c, p)
	}
	n := g.Nodes[c]
	if !slices.Contains(n.Blocks, p) {
		n.Blocks = append(n.Blocks, p)
		appendLog(n, fmt.Sprintf("now blocks `%s`", p), who(a))
	}
	g.mutate(n)
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
	g.mutate(changed...)
	fmt.Println("removed", i)
}

func cmdEdit(a *Args) {
	need(a, 1, "edit")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], filepath.Join(g.Dir, "nodes", i+".md"))...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
	writeGraphMD(g.Dir)
}

func cmdLs(a *Args) {
	g := load(requireTree())
	if a.Bool("json") {
		printJSON(g.graphJSON(false))
		return
	}
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
	i := g.resolve(a.Pos[0])
	n, f := g.Nodes[i], g.Info[i]

	var chain []string // follow first parent up to a head for the "why" chain
	for cur := f.Parents; len(cur) > 0; cur = g.Info[cur[0]].Parents {
		chain = append(chain, cur[0])
	}

	if a.Bool("json") {
		d := g.nodeJSON(i, true)
		d.Chain, d.Children = []NodeJSON{}, []NodeJSON{}
		for _, p := range chain {
			d.Chain = append(d.Chain, g.nodeJSON(p, false))
		}
		for _, c := range f.Children {
			d.Children = append(d.Children, g.nodeJSON(c, false))
		}
		printJSON(d)
		return
	}

	fmt.Println(n.Title)
	fmt.Printf("  id: %s   state: %s   kind: %s   unblocks: %d\n", i, f.Label, f.Kind, f.Unblocks)
	if n.Mode != "" || n.Agent != "" {
		fmt.Printf("  mode: %s   agent: %s\n", dash(n.Mode), dash(n.Agent))
	}
	if n.Outcome != "" {
		fmt.Println("  outcome:", n.Outcome)
	}
	if len(n.Touches) > 0 {
		fmt.Println("  touches:", strings.Join(n.Touches, ", "))
	}
	if n.At != "" {
		fmt.Println("  resolved at:", short(n.At))
	}
	if len(n.Stale) > 0 {
		fmt.Println("  STALE, code changed since:", strings.Join(n.Stale, ", "))
	}
	if len(chain) > 0 {
		fmt.Println("\nWhy (chain to head):")
		for d, p := range chain {
			fmt.Printf("  %s↑ %s\n", strings.Repeat("  ", d), g.line(p))
		}
		if len(f.Parents) > 1 {
			fmt.Println("  also blocks:", strings.Join(f.Parents[1:], ", "))
		}
	}
	if len(f.Children) > 0 {
		fmt.Println("\nWaits on:")
		for _, c := range f.Children {
			fmt.Println("  -", g.line(c))
			if o := g.Nodes[c].Outcome; o != "" {
				fmt.Println("      ⇒", o)
			}
		}
	}
	fmt.Println("\n" + strings.TrimSpace(n.Body))
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
		if k, err := strconv.Atoi(s); err == nil && k < len(ids) {
			ids = ids[:k]
		}
	}
	if a.Bool("json") {
		out := []NodeJSON{}
		for _, i := range ids {
			out = append(out, g.nodeJSON(i, false))
		}
		printJSON(out)
		return
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
	if a.Bool("json") {
		format = "json"
	}
	switch format {
	case "json":
		printJSON(load(dir).graphJSON(a.Bool("full")))
	case "mermaid":
		g := load(dir)
		fmt.Println(g.mermaid(g.IDs))
	case "md":
		writeGraphMD(dir)
		data, _ := os.ReadFile(filepath.Join(dir, "GRAPH.md"))
		os.Stdout.Write(data)
	default:
		die("--format must be md, json or mermaid")
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
			c.run(parseArgs(name, os.Args[2:], c.flags))
			return
		}
	}
	die("unknown command '%s' (see `ct --help`)", name)
}
