package main

// The conversation tree. Every package you work on gets a tree of the topics
// you and Claude go through in it: each HEAD is a topic in that package
// ("Maps and sets in Std"), threads are its sub-topics. A Stop hook hands each
// finished turn to a background worker. The worker finds the package from the
// files Claude read or edited in that turn, asks a small model where the turn
// belongs, and updates that package's tree.
//
// Nothing is written to the code repo. Everything is plain text under
// ~/.local/state/code-tree:
//   repos/<repo>/<package>/.tree/nodes/*.md   one tree per package (ct's node format)
//   repos/<repo>/<package>/pkg                "repo <root>" / "package <path>"
//   sessions/<session-id>/convo                where a session is: repo, package, thread, turns

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const classifyModel = "haiku"

func convoHome() string {
	if h := os.Getenv("CT_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "code-tree")
}

func sessionDir(id string) string { return filepath.Join(convoHome(), "sessions", filepath.Base(id)) }

// ---------------------------------------------------------------- repos and packages

func repoRoot(cwd string) string {
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return cwd
}

func repoSlug(root string) string {
	sum := sha1.Sum([]byte(root))
	return slugify(filepath.Base(root)) + "-" + hex.EncodeToString(sum[:3])
}

func pkgSlug(rel string) string {
	if rel == "" || rel == "." {
		return "_root"
	}
	return strings.ReplaceAll(rel, "/", "__")
}

// pkgTree is the tree directory for one package of one repo, created on demand.
func pkgTree(root, rel string) string {
	dir := filepath.Join(convoHome(), "repos", repoSlug(root), pkgSlug(rel))
	t := filepath.Join(dir, treeDir)
	if _, err := os.Stat(filepath.Join(t, "nodes")); err != nil {
		_ = os.MkdirAll(filepath.Join(t, "nodes"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "pkg"), []byte(fmt.Sprintf("repo %s\npackage %s\n", root, rel)), 0o644)
	}
	return t
}

// pkgName is how a package is shown: its folder, or the repo name at the root.
func pkgName(root, rel string) string {
	if rel == "" || rel == "." {
		return filepath.Base(root)
	}
	return rel
}

// packageOf picks the package a turn worked in: the folder most of its files are in.
func packageOf(root string, files []string, fallback string) string {
	count := map[string]int{}
	last := ""
	for _, f := range files {
		if !filepath.IsAbs(f) {
			continue
		}
		rel, err := filepath.Rel(root, f)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if strings.HasPrefix(dir, ".") && dir != "." { // .git, .tree, .claude ...
			continue
		}
		if !codeExt[strings.ToLower(filepath.Ext(f))] { // docs and config don't make a package
			continue
		}
		count[dir]++
		last = dir
	}
	if last == "" {
		return fallback
	}
	best := last
	dirs := make([]string, 0, len(count))
	for d := range count {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // ties always break the same way
	for _, d := range dirs {
		if count[d] > count[best] {
			best = d
		}
	}
	return best
}

// repoPackages lists the packages of a repo that have a tree, busiest first.
func repoPackages(root string) []string {
	base := filepath.Join(convoHome(), "repos", repoSlug(root))
	entries, _ := os.ReadDir(base)
	type pk struct {
		rel  string
		when time.Time
	}
	var out []pk
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(base, e.Name(), "pkg"))
		if err != nil {
			continue
		}
		rel := ""
		for _, l := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(l, " "); ok && k == "package" {
				rel = v
			}
		}
		var when time.Time
		if info, err := os.Stat(filepath.Join(base, e.Name(), treeDir, "GRAPH.md")); err == nil {
			when = info.ModTime()
		}
		out = append(out, pk{rel, when})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	var rels []string
	for _, p := range out {
		rels = append(rels, p.rel)
	}
	return rels
}

// ---------------------------------------------------------------- session state

type convoState map[string]string

func readConvo(id string) convoState {
	st := convoState{}
	f, err := os.Open(filepath.Join(sessionDir(id), "convo"))
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok {
			st[k] = v
		}
	}
	return st
}

func (st convoState) save(id string) {
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", k, oneLine(st[k]))
	}
	_ = os.MkdirAll(sessionDir(id), 0o755)
	p := filepath.Join(sessionDir(id), "convo")
	_ = os.WriteFile(p+".tmp", []byte(b.String()), 0o644)
	_ = os.Rename(p+".tmp", p)
}

// ---------------------------------------------------------------- transcript

type turn struct {
	prompt, reply string
	files         []string
}

// readTurns pulls each human prompt, the agent's text replies and the files its
// tools touched out of a Claude Code transcript (JSONL, Claude Code's format).
func readTurns(path string) []turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var turns []turn
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var l struct {
				Type       string `json:"type"`
				IsMeta     bool   `json:"isMeta"`
				Compact    bool   `json:"isCompactSummary"`          // a compaction summary, not the human
				Transcript bool   `json:"isVisibleInTranscriptOnly"` // shown in the transcript, never typed
				Message    struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
				Attachment struct {
					HookEvent string `json:"hookEvent"`
					Content   string `json:"content"`
				} `json:"attachment"`
			}
			if json.Unmarshal(line, &l) == nil && !l.IsMeta && !l.Compact && !l.Transcript {
				// an intercom message delivered with a prompt is part of what the human said
				if l.Attachment.HookEvent == "UserPromptSubmit" && strings.HasPrefix(l.Attachment.Content, "[intercom]") && len(turns) > 0 {
					turns[len(turns)-1].prompt += "\n" + l.Attachment.Content
					continue
				}
				switch l.Type {
				case "user":
					if p := userText(l.Message.Content); p != "" {
						turns = append(turns, turn{prompt: p})
					}
				case "assistant":
					if len(turns) > 0 {
						text, files := assistantParts(l.Message.Content)
						last := &turns[len(turns)-1]
						if text != "" {
							last.reply = strings.TrimSpace(last.reply + "\n" + text)
						}
						last.files = append(last.files, files...)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return turns
}

func userText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return cleanPrompt(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, p.Text)
		case "tool_result":
			return "" // tool output, not the human
		}
	}
	return cleanPrompt(strings.Join(out, "\n"))
}

// cleanPrompt drops harness wrappers so only what the human typed remains.
func cleanPrompt(s string) string {
	for _, tag := range []string{"system-reminder", "local-command-stdout", "local-command-caveat", "bash-stdout", "bash-stderr"} {
		for {
			a := strings.Index(s, "<"+tag)
			if a < 0 {
				break
			}
			b := strings.Index(s[a:], "</"+tag+">")
			if b < 0 {
				s = s[:a]
				break
			}
			s = s[:a] + s[a+b+len(tag)+3:]
		}
	}
	s = strings.TrimSpace(s)
	for _, notHuman := range []string{"<command-", "[Request interrupted", "Caveat:", "<task-notification>",
		"[SYSTEM NOTIFICATION", "This session is being continued from a previous conversation"} {
		if strings.HasPrefix(s, notHuman) {
			return ""
		}
	}
	// a slash command ("/code-review max") runs a tool; it is not a topic
	if strings.HasPrefix(s, "/") && !strings.Contains(s, "\n") && len(strings.Fields(s)) <= 4 {
		return ""
	}
	return s
}

func assistantParts(raw json.RawMessage) (string, []string) {
	var parts []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var text, files []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			text = append(text, p.Text)
		case "tool_use":
			var in struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
				Path         string `json:"path"`
			}
			if json.Unmarshal(p.Input, &in) == nil {
				for _, f := range []string{in.FilePath, in.NotebookPath, in.Path} {
					if f != "" {
						files = append(files, f)
					}
				}
			}
		}
	}
	return strings.Join(text, "\n"), files
}

func clip(s string, n int, keepEnd bool) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if keepEnd {
		return "…" + string(r[len(r)-n:])
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------- placing a turn

type placement struct {
	thread, parent, topic, title, status, summary string
	close                                         []string
}

func classifyPrompt(g *Graph, pkg string, t turn) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You keep a tree of what a developer and a coding agent discuss about one package of code: %s.
Each HEAD is a topic in that package. Threads are its sub-topics, and can branch further.
Place the newest turn in the tree.

Current tree (id | parent | status | title | summary):
`, pkg)
	if len(g.IDs) == 0 {
		b.WriteString("(empty: start the first topic)\n")
	}
	for _, i := range g.IDs {
		n := g.Nodes[i]
		parent := "-"
		if len(g.Info[i].Parents) > 0 {
			parent = g.Info[i].Parents[0]
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s\n", i, parent, n.Status, n.Title, n.Outcome)
	}
	fmt.Fprintf(&b, "\nNewest turn\nDEVELOPER: %s\nAGENT: %s\n", clip(t.prompt, 1500, false), clip(t.reply, 2500, true))
	b.WriteString(`
Reply with exactly these lines and nothing else:
thread: <id of the existing thread this turn continues, or new>
parent: <for a new thread: id of the thread or HEAD it branches from, or new-topic>
topic: <only when parent is new-topic: 3 to 7 words naming the new topic>
title: <3 to 7 words naming the thread>
status: <open | needs-you | ready>
summary: <one line: where this thread stands now; for needs-you, the exact question the agent asked, on one line>
close: <ids of other threads this turn settled, comma separated, or none>

Rules:
- Keep the tree wide and shallow, like an outline. A new thread's parent is usually the HEAD.
  Nest under a thread only when the turn is a sub-question of that thread specifically,
  never just because it came next in time.
- Continue an existing thread when the turn is still about it.
- Start a new topic only when the turn is unrelated to every HEAD.
- When the conversation moves on from a thread that reached its outcome, list it in close.
- status is strict:
  needs-you ONLY when the agent stopped because it cannot go on without the developer: it asked
    them to choose between options, or to approve something it will not do alone (push, delete,
    publish, spend), or it needs information only they have.
  ready when the work or discussion reached an outcome, EVEN IF the agent ends with an optional
    offer such as "want me to...?", "say if you'd like...", "next I could...", "let me know",
    "should I build X next?". Offers, suggestions and invitations to test are never needs-you.
  open while it is still being worked on or discussed.
- Re-judge every thread marked needs-you in the tree: if the developer's newest message answered
  it or moved past it, or it was only an optional offer, list it in close.
- Every value stays on its one line; never start a new line inside a value.
- A HEAD is never the thread itself. Titles name the subject, not the latest event.
`)
	return b.String()
}

func parsePlacement(out string) placement {
	var p placement
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		if seen[key] {
			continue // the first answer counts; a copied line like "Status: ..." must not overwrite it
		}
		seen[key] = true
		v = unwrapValue(v)
		switch key {
		case "thread":
			p.thread = v
		case "parent":
			p.parent = v
		case "topic":
			p.topic = v
		case "title":
			p.title = v
		case "status":
			p.status = strings.ToLower(v)
		case "summary":
			p.summary = v
		case "close":
			if !strings.EqualFold(v, "none") {
				for _, c := range strings.Split(v, ",") {
					if c = strings.Trim(strings.TrimSpace(c), "`*\"' "); c != "" {
						p.close = append(p.close, c)
					}
				}
			}
		}
	}
	switch p.status {
	case "open", "needs-you", "ready":
	case "done", "settled", "closed", "resolved", "finished":
		p.status = "ready" // the tree shows settled nodes as done; read that back as settled
	default:
		p.status = "open"
	}
	return p
}

// unwrapValue trims a reply value and removes one pair of wrapping quotes,
// backticks or <> placeholders, leaving quotes inside the text alone.
func unwrapValue(v string) string {
	v = strings.TrimSpace(v)
	for _, pair := range []string{"``", `""`, "<>", "''"} {
		if len(v) >= 2 && v[0] == pair[0] && v[len(v)-1] == pair[1] {
			return strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}

// classify asks the small model where a turn goes. Its own session must not
// join intercom or code-tree, hence the skip variables.
func classify(prompt, cwd string) (string, error) { return askModel(classifyModel, prompt, cwd) }

// askModel runs a one-shot claude -p with the given model.
func askModel(model, prompt, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", model)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = cwd
	cmd.Env = helperEnv()
	out, err := cmd.Output()
	return string(out), err
}

func newNode(g *Graph, title, agent string, blocks []string) *Node {
	base := slugify(title)
	id := base
	for k := 2; g.Nodes[id] != nil || nodeFileExists(g.Dir, id); k++ {
		id = fmt.Sprintf("%s-%d", base, k)
	}
	n := &Node{ID: id, Title: title, Mode: "discuss", Agent: agent, Status: "open", Blocks: blocks,
		Created: now(), Extra: map[string]string{}, Body: "\n## Context\n\n## Log\n"}
	g.Nodes[id], g.IDs = n, append(g.IDs, id)
	return n
}

// apply puts a turn into a package tree and returns the thread it landed on.
func apply(tree, agent string, p placement) string {
	g := load(tree)
	changed := map[string]*Node{}
	mark := func(n *Node) { changed[n.ID] = n }
	n := g.Nodes[p.thread]
	named := "" // a subject the model pointed at as the thread: file the turn under it
	if n != nil && g.Info[p.thread].Kind == "head" {
		named, n = p.thread, nil
	}
	if n == nil {
		parent := p.parent
		if g.Nodes[parent] == nil && named != "" {
			parent = named
		}
		if g.Nodes[parent] == nil || parent == "new-topic" {
			heads := g.heads()
			if parent == "new-topic" || len(heads) == 0 {
				topic := p.topic
				if topic == "" {
					topic = p.title
				}
				if topic == "" {
					topic = "Topic"
				}
				h := newNode(g, topic, agent, []string{})
				mark(h)
				parent = h.ID
			} else {
				parent = heads[len(heads)-1]
			}
		}
		title := p.title
		if title == "" {
			title = clip(p.summary, 40, false)
		}
		n = newNode(g, title, agent, []string{parent})
		// a new point under a settled one means it is open again
		if pn := g.Nodes[parent]; pn != nil && g.Info[parent] != nil && g.Info[parent].Kind != "head" &&
			(pn.Status == "done" || pn.Status == "ready") {
			pn.Status = "open"
			appendLog(pn, "reopened: new point `"+n.ID+"` under it", agent)
			mark(pn)
		}
	}
	wasWaiting := n.Status == "needs-you"
	n.Status = p.status
	switch {
	case p.summary != "":
		n.Outcome = p.summary
	case wasWaiting && p.status != "needs-you":
		n.Outcome = "Answered." // never leave the old question as the outcome
	}
	appendLog(n, plainStatusWord(p.status)+": "+p.summary, agent)
	mark(n)
	for _, c := range p.close {
		cn := g.Nodes[c]
		if cn == nil || c == n.ID || g.Info[c] == nil || g.Info[c].Kind == "head" || resolved(cn.Status) || cn.Status == "ready" {
			continue
		}
		if cn.Status == "needs-you" {
			cn.Outcome = "Answered: " + p.summary
		}
		cn.Status = "ready"
		appendLog(cn, "settled", agent)
		mark(cn)
	}
	settle(g, mark)
	var list []*Node
	for _, c := range changed {
		list = append(list, c)
	}
	g.mutate(list...)
	return n.ID
}

func plainStatusWord(s string) string {
	switch s {
	case "ready", "done":
		return "settled"
	case "needs-you":
		return "waiting for you"
	}
	return s
}

// settle keeps settled-ness honest across the tree: a settled point counts as
// done only when nothing under it is still open (until then it shows "k of n
// settled"), and a subject is settled exactly when all its points are.
func settle(g *Graph, mark func(*Node)) {
	for pass := 0; pass <= len(g.IDs); pass++ {
		g.compute()
		moved := false
		for _, i := range g.IDs {
			n, f := g.Nodes[i], g.Info[i]
			open := false
			for _, c := range f.Children {
				if !resolved(g.Nodes[c].Status) {
					open = true
				}
			}
			want := n.Status
			switch {
			case f.Kind == "head" && len(f.Children) > 0 && (n.Status == "open" || n.Status == "done"):
				want = "open"
				if !open {
					want = "done"
				}
			case f.Kind != "head" && (n.Status == "ready" || n.Status == "done"):
				want = "ready"
				if !open {
					want = "done"
				}
			}
			if want != n.Status {
				n.Status = want
				mark(n)
				moved = true
			}
		}
		if !moved {
			return
		}
	}
}

// ---------------------------------------------------------------- worker

const maxBackfill = 30

// cmdTurn places every turn of a session that is not in a tree yet. One
// worker per session at a time; a second one just leaves.
func cmdTurn(a *Args) {
	id, path, cwd := a.Get("session", ""), a.Get("transcript", ""), a.Get("cwd", "")
	if id == "" || path == "" {
		die("usage: ct turn --session ID --transcript PATH [--cwd DIR]")
	}
	_ = os.MkdirAll(sessionDir(id), 0o755)
	lock := filepath.Join(sessionDir(id), "lock")
	if f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprint(f, os.Getpid())
		f.Close()
	} else if data, _ := os.ReadFile(lock); pidAlive(string(data)) {
		return
	} else {
		_ = os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	defer os.Remove(lock)

	st := readConvo(id)
	if cwd == "" {
		cwd = st["cwd"]
	}
	root := repoRoot(cwd)
	agent := st["agent"]
	if agent == "" {
		agent = icNameFor(id)
	}
	pkg, ok := st["package"]
	if !ok {
		pkg = "."
	}
	updateConvo(id, func(st convoState) {
		st["transcript"], st["cwd"], st["repo"], st["agent"] = path, cwd, root, agent
	})
	for {
		turns := readTurns(path)
		st := readConvo(id)
		done, _ := strconv.Atoi(st["turns"])
		// only turns that have ended: the stop hook records how many there are
		complete, err := strconv.Atoi(st["complete"])
		if err != nil || complete > len(turns) {
			complete = len(turns)
		}
		if done < complete-maxBackfill {
			done = complete - maxBackfill
		}
		if done >= complete {
			break
		}
		t := turns[done]
		var landed, tree string
		if strings.TrimSpace(t.reply) != "" { // a turn with no answer (a tool command) is skipped
			rel := packageOf(root, t.files, pkg)
			if tagged, ok := taggedPackage(t.prompt, root); ok {
				rel = tagged // the human pressed t or g on a box in that package's graph
			}
			tree = pkgTree(root, rel)
			g := load(tree)
			out, err := classify(classifyPrompt(g, pkgName(root, rel), t), cwd)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s classify turn %d: %v\n", now(), done, err)
				break
			}
			p := parsePlacement(out)
			if id := pointedAt(t.prompt, g); id != "" {
				p.thread = id // the human pressed t on this box: no guessing
			}
			landed, pkg = apply(tree, agent, p), rel
		}
		updateConvo(id, func(st convoState) {
			if landed != "" {
				st["current"], st["package"], st["tree"] = landed, pkg, tree
			}
			st["turns"] = strconv.Itoa(done + 1)
			st["updated"] = now()
		})
	}
}

// The graph pane tags t and g messages "[code-tree · <package> · <id> · <title>]"
// or "[code-tree · <package> · <id>] ...". The package is a folder name and may
// hold "]" or " · " itself, so every " · <id>" in the tag line is a candidate
// split; the caller says which split is real (a node or package that exists).
var tagID = regexp.MustCompile(`^[a-z0-9-]+(?: · |\])`)

// tagSplits lists the (package, id) readings of the tag in prompt, first to last.
func tagSplits(prompt string) [][2]string {
	const open = "[code-tree · "
	k := strings.Index(prompt, open)
	if k < 0 {
		return nil
	}
	line, _, _ := strings.Cut(prompt[k+len(open):], "\n")
	var out [][2]string
	for at := 0; ; {
		j := strings.Index(line[at:], " · ")
		if j < 0 {
			return out
		}
		j += at
		if j > 0 {
			if m := tagID.FindString(line[j+len(" · "):]); m != "" {
				id := strings.TrimSuffix(strings.TrimSuffix(m, " · "), "]")
				out = append(out, [2]string{line[:j], id})
			}
		}
		at = j + len(" · ")
	}
}

// pointedAt is the box a message sent from the graph pane (t or g) was about.
func pointedAt(prompt string, g *Graph) string {
	for _, s := range tagSplits(prompt) {
		if g.Nodes[s[1]] != nil && g.Info[s[1]].Kind != "head" {
			return s[1]
		}
	}
	return ""
}

// taggedPackage is the package a graph pane message names, as one of this
// repo's known packages (its rel path); false when there is no tag or no such package.
func taggedPackage(prompt, root string) (string, bool) {
	splits := tagSplits(prompt)
	if len(splits) == 0 {
		return "", false
	}
	for _, rel := range repoPackages(root) {
		for _, s := range splits {
			if pkgName(root, rel) == s[0] {
				return rel, true
			}
		}
	}
	return "", false
}

// updateConvo changes a session's state from a fresh read, so the hooks and
// the worker never undo each other's writes.
func updateConvo(id string, change func(convoState)) {
	st := readConvo(id)
	change(st)
	st.save(id)
}

func pidAlive(s string) bool {
	pid, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// icNameFor is the session's intercom name, read from intercom's plain state.
func icNameFor(id string) string {
	if s := icSession(id); s["name"] != "" {
		return s["name"]
	}
	return "claude"
}

func icHome() string {
	if h := os.Getenv("IC_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "intercom")
}

func icSession(id string) map[string]string {
	kv := map[string]string{}
	data, err := os.ReadFile(filepath.Join(icHome(), "sessions", filepath.Base(id)))
	if err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(l, " "); ok {
				kv[k] = v
			}
		}
	}
	return kv
}

// ---------------------------------------------------------------- hooks

// cmdHook is Claude Code's entry: "prompt" marks the session busy, "stop"
// starts a worker for the finished turn. Both return at once.
func cmdHook(a *Args) {
	if os.Getenv("CT_SKIP") == "1" || len(a.Pos) == 0 {
		return
	}
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
		Reason         string `json:"reason"`
	}
	data, _ := io.ReadAll(os.Stdin)
	if json.Unmarshal(data, &in) != nil || in.SessionID == "" {
		return
	}
	switch a.Pos[0] {
	case "prompt":
		updateConvo(in.SessionID, func(st convoState) { st["busy"], st["cwd"] = now(), in.Cwd })
	case "end":
		updateConvo(in.SessionID, func(st convoState) { delete(st, "busy") })
		closeTab(in.SessionID, in.Reason)
	case "stop":
		// the turn just ended: every turn in the transcript so far is complete
		complete := strconv.Itoa(len(readTurns(in.TranscriptPath)))
		updateConvo(in.SessionID, func(st convoState) {
			delete(st, "busy")
			st["complete"] = complete
		})
		exe, err := os.Executable()
		if err != nil {
			return
		}
		logf, _ := os.OpenFile(filepath.Join(sessionDir(in.SessionID), "worker.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		cmd := exec.Command(exe, "turn", "--session", in.SessionID, "--transcript", in.TranscriptPath, "--cwd", in.Cwd)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = cmd.Start()
	}
}

// ---------------------------------------------------------------- install

const installFilter = `
def ct_entry($cmd): {hooks: [{type: "command", command: $cmd, timeout: 10}]};
.hooks //= {}
| reduce $evs[] as $e (.;
    .hooks[$e.event] = (
      (.hooks[$e.event] // [])
      | map(.hooks |= map(select((.command // "") | test("(^|/)ct hook ") | not)))
      | map(select((.hooks | length) > 0))
    ) + [ct_entry($e.cmd)]
  )`

func cmdInstall(a *Args) {
	if len(a.Pos) == 0 || a.Pos[0] != "claude" {
		die("usage: ct install claude")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		die("needs jq (brew install jq)")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil || strings.Contains(exe, "go-build") {
		die("run an installed ct (go install ./cmd/ct), not `go run`")
	}
	h, _ := os.UserHomeDir()
	path := filepath.Join(h, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data = []byte("{}")
	} else if err != nil {
		die("%v", err)
	}
	backup := fmt.Sprintf("%s.bak-ct-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		die("%v", err)
	}
	evs := fmt.Sprintf(`[{"event":"UserPromptSubmit","cmd":%q},{"event":"Stop","cmd":%q},{"event":"SessionEnd","cmd":%q}]`, exe+" hook prompt", exe+" hook stop", exe+" hook end")
	cmd := exec.Command("jq", "--argjson", "evs", evs, installFilter)
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.Output()
	if err != nil {
		die("jq failed: %v", err)
	}
	if err := os.WriteFile(path+".tmp", out, 0o644); err != nil {
		die("%v", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		die("%v", err)
	}
	fmt.Printf("added ct hooks to %s (backup: %s)\nNew turns in every Claude session now build their package's tree.\n", path, backup)
}

// cmdSessions lists sessions with a tree, newest first.
func cmdSessions(a *Args) {
	entries, _ := os.ReadDir(filepath.Join(convoHome(), "sessions"))
	type row struct{ id, updated, agent, where string }
	var rows []row
	for _, e := range entries {
		st := readConvo(e.Name())
		if st["turns"] == "" {
			continue
		}
		rows = append(rows, row{e.Name(), st["updated"], st["agent"], pkgName(st["repo"], st["package"])})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].updated > rows[j].updated })
	for _, r := range rows {
		fmt.Printf("%s  %-16s %s  %s\n", short(r.id), r.agent, r.updated, r.where)
	}
}

// closeTab closes the herdr tab of a Claude session that just exited, so its
// tree pane goes with it. Off by default: quitting Claude leaves the tab and its
// shell. CT_CLOSE_TAB=1 turns it on. /clear and /resume end a session inside
// the same Claude, so those always keep the tab.
//
// It only ever closes a tab whose pane herdr reports as running this very
// session, so a stray "session ended" from any other process (a helper, a
// child that inherited the pane's environment) can never close a tab.
// Every decision is logged to tabclose.log.
func closeTab(session, reason string) {
	logf := func(format string, a ...any) {
		f, err := os.OpenFile(filepath.Join(convoHome(), "tabclose.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%s session=%s reason=%s pane=%s ppid=%d: %s\n", now(), short(session), reason,
				os.Getenv("HERDR_PANE_ID"), os.Getppid(), fmt.Sprintf(format, a...))
			f.Close()
		}
	}
	if os.Getenv("CT_CLOSE_TAB") != "1" {
		return
	}
	if reason == "clear" || reason == "resume" {
		logf("kept: %s", reason)
		return
	}
	bin, pane := herdrBin(), os.Getenv("HERDR_PANE_ID")
	if bin == "" || pane == "" {
		logf("kept: not in herdr")
		return
	}
	out, err := exec.Command(bin, "pane", "get", pane).Output()
	if err != nil {
		logf("kept: pane lookup failed")
		return
	}
	var res struct {
		Result struct {
			Pane struct {
				TabID        string `json:"tab_id"`
				AgentSession struct {
					Value string `json:"value"`
				} `json:"agent_session"`
			} `json:"pane"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &res) != nil || res.Result.Pane.TabID == "" {
		logf("kept: no tab for pane")
		return
	}
	if res.Result.Pane.AgentSession.Value != session {
		logf("kept: pane runs session %q, not this one", short(res.Result.Pane.AgentSession.Value))
		return
	}
	logf("closing tab %s", res.Result.Pane.TabID)
	// after Claude has finished exiting
	cmd := exec.Command("sh", "-c", `sleep 1; exec "$0" tab close "$1"`, bin, res.Result.Pane.TabID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

// codeExt are the files that say which package a turn worked in.
var codeExt = map[string]bool{
	".go": true, ".zig": true, ".rs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".py": true, ".rb": true, ".java": true,
	".kt": true, ".swift": true, ".ml": true, ".mli": true, ".hs": true, ".ex": true, ".exs": true, ".lua": true,
	".odin": true, ".cs": true, ".scala": true, ".php": true, ".sh": true, ".gd": true, ".sql": true,
}

// cmdRecheck re-judges every box marked "waiting for you" in every tree with
// the strict rule, and settles the ones that were only offers or are answered.
func cmdRecheck(a *Args) {
	trees, _ := filepath.Glob(filepath.Join(convoHome(), "repos", "*", "*", treeDir))
	settled, kept := 0, 0
	for _, tree := range trees {
		g := load(tree)
		var fixed []*Node // trees saved before settling was consistent
		settle(g, func(n *Node) { fixed = append(fixed, n) })
		if len(fixed) > 0 {
			g.mutate(fixed...)
			g = load(tree)
		}
		var waiting []string
		var b strings.Builder
		b.WriteString(`Each item below is a thread from a developer's conversation with a coding agent that is
marked "waiting for the developer". Decide for each whether that is really true.

keep: the agent stopped because it cannot go on without the developer: it asked them to choose
  between options, to approve something it will not do alone (push, delete, publish, spend), or
  for information only they have.
settled: the work reached an outcome and the agent only offered optional next steps ("want me
  to...?", "say if...", "next I could...", "let me know", invitations to test), or the question
  was already answered or overtaken later in the conversation.

`)
		for _, i := range g.IDs {
			n := g.Nodes[i]
			if n.Status != "needs-you" {
				continue
			}
			waiting = append(waiting, i)
			fmt.Fprintf(&b, "id: %s\ntitle: %s\nsummary: %s\nrecent history:\n", i, n.Title, n.Outcome)
			var logs []string
			for _, l := range strings.Split(n.Body, "\n") {
				if m := logLine.FindStringSubmatch(l); m != nil {
					logs = append(logs, "  "+m[2])
				}
			}
			if len(logs) > 3 {
				logs = logs[len(logs)-3:]
			}
			b.WriteString(strings.Join(logs, "\n") + "\n\n")
		}
		if len(waiting) == 0 {
			continue
		}
		// what happened afterwards tells whether a question was overtaken
		b.WriteString("Everything discussed in this code folder, oldest first (later threads may have answered or overtaken an item):\n")
		all := append([]string{}, g.IDs...)
		sort.Slice(all, func(x, y int) bool { return g.Nodes[all[x]].Updated < g.Nodes[all[y]].Updated })
		for _, i := range all {
			n := g.Nodes[i]
			fmt.Fprintf(&b, "- %s [%s, last touched %s]: %s\n", n.Title, n.Status, n.Updated, n.Outcome)
		}
		if said := recentPrompts(filepath.Dir(tree), 25); said != "" {
			b.WriteString("\nWhat the developer said most recently in this repo, oldest first:\n" + said)
		}
		b.WriteString("\nReply with one line per item and nothing else: <id>: keep  or  <id>: settled\n")
		out, err := askModel("sonnet", b.String(), filepath.Dir(tree)) // rare and judgment-heavy: a stronger model
		if os.Getenv("CT_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "--- %s\n%s\n", tree, out)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", tree, err)
			continue
		}
		verdict := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if id, v, ok := strings.Cut(line, ":"); ok {
				verdict[strings.Trim(strings.TrimSpace(id), "`*-")] = strings.ToLower(strings.TrimSpace(v))
			}
		}
		g = load(tree) // the model call took a while: apply to what is on disk now
		var changed []*Node
		for _, i := range waiting {
			n := g.Nodes[i]
			if n == nil || n.Status != "needs-you" {
				continue // changed meanwhile
			}
			if strings.HasPrefix(verdict[i], "settled") {
				appendLog(n, "settled on recheck: it was not really waiting on you (it said: "+n.Outcome+")", "ct recheck")
				n.Status, n.Outcome = "ready", "Settled on recheck."
				changed = append(changed, n)
				settled++
				fmt.Printf("settled  %s\n", n.Title)
			} else {
				kept++
				fmt.Printf("waiting  %s\n", n.Title)
			}
		}
		if len(changed) > 0 {
			settle(g, func(n *Node) {
				if !slices.Contains(changed, n) {
					changed = append(changed, n)
				}
			})
			g.mutate(changed...)
		}
	}
	fmt.Printf("\n%d settled, %d still waiting for you\n", settled, kept)
}

// recentPrompts is what the developer said lately in the repo a package tree
// belongs to, across its sessions: enough to tell an answered question.
func recentPrompts(pkgDir string, n int) string {
	data, err := os.ReadFile(filepath.Join(pkgDir, "pkg"))
	if err != nil {
		return ""
	}
	root := ""
	for _, l := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(l, " "); ok && k == "repo" {
			root = v
		}
	}
	entries, _ := os.ReadDir(filepath.Join(convoHome(), "sessions"))
	var sts []convoState
	for _, e := range entries {
		if st := readConvo(e.Name()); st["repo"] == root && st["transcript"] != "" {
			sts = append(sts, st)
		}
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i]["updated"] < sts[j]["updated"] }) // oldest session first, so the newest messages are last
	var prompts []string
	for _, st := range sts {
		for _, t := range readTurns(st["transcript"]) {
			prompts = append(prompts, "- "+clip(oneLine(t.prompt), 220, false))
		}
	}
	if len(prompts) > n {
		prompts = prompts[len(prompts)-n:]
	}
	return strings.Join(prompts, "\n") + "\n"
}

// helperEnv is the environment for a helper `claude -p` (placing turns, node
// chats): it must not build a conversation tree of its own, join intercom, or
// look like the agent of the herdr pane it was started from.
func helperEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HERDR_") || k == "IC_ID" || k == "CLAUDECODE" {
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "CT_SKIP=1", "IC_SKIP=1"), extra...)
}
