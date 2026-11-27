package main

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const treeDir = ".tree"

var (
	statuses = []string{"proposed", "open", "working", "needs-you", "ready", "done", "dropped"}
	labels   = map[string]string{
		"waiting":   "Waiting",
		"stale":     "Stale",
		"open":      "Open",
		"working":   "Working",
		"needs-you": "Needs you",
		"proposed":  "Proposed",
		"ready":     "Ready",
		"done":      "Done",
		"dropped":   "Dropped",
	}
	fieldOrder = []string{"id", "title", "mode", "agent", "status", "blocks", "outcome", "created", "updated"}
)

func resolved(status string) bool { return status == "done" || status == "dropped" }

func label(s string) string {
	if l, ok := labels[s]; ok {
		return l
	}
	return s
}

// Node is one .tree/nodes/<id>.md file.
type Node struct {
	ID, Title, Mode, Agent, Status, Outcome, Created, Updated string
	Blocks                                                    []string
	Touches                                                   []string          // code paths/patterns this node owns
	Stale                                                     []string          // paths changed since it was resolved
	At, Base                                                  string            // code commit at resolution / at claim
	Check                                                     string            // shell command that must pass before ready/done
	Decisions                                                 []string          // constraints that hold for this node and everything below
	Extra                                                     map[string]string // unknown fields, kept verbatim
	Body                                                      string
}

// Info is everything derived from the graph shape.
type Info struct {
	State, Label, Kind string
	Children, Parents  []string
	Ancestors          []string
	Unblocks           int
}

type Graph struct {
	Dir   string // path to .tree
	Nodes map[string]*Node
	IDs   []string // sorted
	Info  map[string]*Info
}

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ct: "+format+"\n", a...)
	os.Exit(1)
}

func findTree() string {
	if env := os.Getenv("CT_TREE"); env != "" { // tree kept outside the code repo
		if filepath.Base(env) != treeDir {
			env = filepath.Join(env, treeDir)
		}
		if st, err := os.Stat(filepath.Join(env, "nodes")); err == nil && st.IsDir() {
			return env
		}
		die("CT_TREE=%s has no nodes/ directory", env)
	}
	dir, err := os.Getwd()
	if err != nil {
		die("%v", err)
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, treeDir, "nodes")); err == nil && st.IsDir() {
			return filepath.Join(dir, treeDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func requireTree() string {
	t := findTree()
	if t == "" {
		die("no .tree/ found here or in any parent. Run `ct init` first.")
	}
	if abs, err := filepath.Abs(t); err == nil { // a relative CT_TREE must survive a change of folder
		t = abs
	}
	if lockWanted {
		lockTree(t)
	}
	return t
}

// ---------------------------------------------------------------- locking

// Commands that change the tree hold an exclusive lock from load to save, so
// two ct runs never overwrite each other's changes. The lock file lives
// outside the tree, so it never shows up in the repo.
var (
	lockWanted bool     // set by main for commands that change the tree
	lockFile   *os.File // held lock, if any
)

func lockPath(tree string) string {
	sum := fmt.Sprintf("%x", sha1.Sum([]byte(tree)))
	return filepath.Join(os.TempDir(), "ct-locks-"+strconv.Itoa(os.Getuid()), sum[:16]+".lock")
}

// lockTree takes the tree's lock (waiting for it) and returns a release
// function. Taking it again while held does nothing.
func lockTree(tree string) (release func()) {
	if lockFile != nil {
		return func() {}
	}
	p := lockPath(tree)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		warnOnce(fmt.Sprintf("cannot lock the tree: %v", err))
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		warnOnce(fmt.Sprintf("cannot lock the tree: %v", err))
		return func() {}
	}
	lockFile = f
	return func() {
		if lockFile == f {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			lockFile = nil
		}
	}
}

// unlocked runs fn (a check, a model call) without holding the lock, so other
// ct commands are not blocked for minutes, then takes the lock back. Callers
// must reload the tree afterwards.
func unlocked(fn func()) {
	if lockFile == nil {
		fn()
		return
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX) }()
	fn()
}

// ---------------------------------------------------------------- parse / dump

func parseNode(text, path string) (*Node, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n") // files saved with Windows line endings
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("%s: missing frontmatter", path)
	}
	rest := text[4:]
	var front, body string
	switch {
	case strings.HasPrefix(rest, "---\n"): // empty frontmatter
		body = rest[4:]
	case rest == "---":
	default:
		end := strings.Index(rest, "\n---\n")
		switch {
		case end >= 0:
			front, body = rest[:end], rest[end+5:]
		case strings.HasSuffix(rest, "\n---"):
			front = rest[:len(rest)-4]
		default:
			return nil, fmt.Errorf("%s: unterminated frontmatter", path)
		}
	}
	n := &Node{
		ID:     strings.TrimSuffix(filepath.Base(path), ".md"),
		Status: "open",
		Extra:  map[string]string{},
		Body:   body,
	}
	fileID := n.ID // the file name is the id: saves go to <id>.md, so an id: line must not point elsewhere
	for _, line := range strings.Split(front, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		str := v // checks and decisions keep their quotes
		switch k {
		case "id":
			if str != fileID {
				warnOnce(fmt.Sprintf("%s: id %q differs from its file name; using %q", filepath.Base(path), str, fileID))
			}
		case "title":
			n.Title = unquote(v) // older files wrote titles and outcomes as quoted strings
		case "mode":
			n.Mode = str
		case "agent":
			n.Agent = str
		case "status":
			n.Status = str
		case "outcome":
			n.Outcome = unquote(v)
		case "created":
			n.Created = str
		case "updated":
			n.Updated = str
		case "at":
			n.At = str
		case "base":
			n.Base = str
		case "check":
			n.Check = str
		case "decision":
			n.Decisions = append(n.Decisions, str)
		case "blocks", "touches", "stale":
			list := splitList(v)
			switch k {
			case "blocks":
				n.Blocks = list
			case "touches":
				n.Touches = list
			default:
				n.Stale = list
			}
		default:
			n.Extra[k] = v
		}
	}
	if n.Title == "" {
		n.Title = n.ID
	}
	if n.Status == "" {
		n.Status = "open"
	}
	if n.Blocks == nil {
		n.Blocks = []string{}
	}
	return n, nil
}

// unquote strips quotes left by older tree files (title: "x").
// Only one whole quoted string is unquoted: `"a" and "b"` stays as it is.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	inner := v[1 : len(v)-1]
	for k := 0; k < len(inner); k++ {
		if inner[k] == '\\' && k+1 < len(inner) {
			k++
		} else if inner[k] == '"' {
			return v // an unescaped quote inside: not one quoted string
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(inner, `\"`, `"`), `\\`, `\`)
}

// quoteIfNeeded writes a title or outcome so unquote reads it back unchanged.
func quoteIfNeeded(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
	}
	return v
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// flat makes a value safe for one frontmatter or log line: control characters
// (newlines, escapes) become spaces, so no value can start a line of its own.
func flat(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, s)
}

// plainValue refuses a value with control characters where one line is required.
func plainValue(what, v string) string {
	if strings.IndexFunc(v, isControl) >= 0 {
		die("%s must be one line of plain text (no newlines or control characters)", what)
	}
	return v
}

// splitList reads "a, b" (and the older ["a", "b"]).
func splitList(v string) []string {
	legacy := strings.HasPrefix(v, "[\"") && strings.HasSuffix(v, "]") // the older ["a", "b"]; a glob like [ab]/x is not
	if legacy {
		v = v[1 : len(v)-1]
	}
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); legacy {
			p = unquote(p)
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dumpNode(n *Node) string {
	vals := map[string]string{
		"id": n.ID, "title": quoteIfNeeded(n.Title), "mode": n.Mode, "agent": n.Agent, "status": n.Status,
		"blocks": strings.Join(n.Blocks, ", "), "outcome": quoteIfNeeded(n.Outcome), "created": n.Created, "updated": n.Updated,
	}
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s: %s\n", k, flat(v)) } // a value never spans lines
	b.WriteString("---\n")
	for _, k := range fieldOrder {
		line(k, vals[k])
	}
	// code-sync fields only appear once used, so plain trees stay plain
	if len(n.Touches) > 0 {
		line("touches", strings.Join(n.Touches, ", "))
	}
	if len(n.Stale) > 0 {
		line("stale", strings.Join(n.Stale, ", "))
	}
	if n.Check != "" {
		line("check", n.Check)
	}
	for _, d := range n.Decisions {
		line("decision", d)
	}
	if n.Base != "" {
		line("base", n.Base)
	}
	if n.At != "" {
		line("at", n.At)
	}
	extra := make([]string, 0, len(n.Extra))
	for k := range n.Extra {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		line(k, n.Extra[k])
	}
	b.WriteString("---\n")
	b.WriteString(n.Body)
	return b.String()
}

// ---------------------------------------------------------------- load / save

func load(dir string) *Graph {
	g := &Graph{Dir: dir, Nodes: map[string]*Node{}}
	// ReadDir, not Glob: a folder name with [ ? * must not change what is read
	entries, _ := os.ReadDir(filepath.Join(dir, "nodes"))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue // editor locks (.#x.md), macOS ._x.md, temp files
		}
		if !validID.MatchString(strings.TrimSuffix(name, ".md")) {
			warnOnce(fmt.Sprintf("skipping %s: a node file name may only use letters, digits, '.', '_' and '-'", name))
			continue
		}
		p := filepath.Join(dir, "nodes", name)
		data, err := os.ReadFile(p)
		if err != nil {
			warnOnce(fmt.Sprintf("skipping %s: %v", name, err))
			continue
		}
		n, err := parseNode(string(data), p)
		if err != nil {
			warnOnce("skipping " + err.Error())
			continue
		}
		g.IDs = append(g.IDs, n.ID) // the id is the file name, so never a duplicate
		g.Nodes[n.ID] = n
	}
	g.compute()
	return g
}

func (g *Graph) save(n *Node) {
	n.Updated = now()
	p := filepath.Join(g.Dir, "nodes", n.ID+".md")
	if err := writeAtomic(p, []byte(dumpNode(n))); err != nil {
		die("%v", err)
	}
}

// writeAtomic replaces a file in one step, so a reader never sees half of it.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), path)
}

var (
	warned   = map[string]bool{}
	warnedMu sync.Mutex
	validID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// warnOnce reports a problem once per run without stopping ct.
func warnOnce(msg string) {
	warnedMu.Lock() // ct serve loads trees from several requests at once
	defer warnedMu.Unlock()
	if !warned[msg] {
		warned[msg] = true
		fmt.Fprintln(os.Stderr, "ct: "+msg)
	}
}

// mutate saves the changed nodes and regenerates GRAPH.md from disk.
func (g *Graph) mutate(changed ...*Node) {
	for _, n := range changed {
		g.save(n)
	}
	writeGraphMD(g.Dir)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		cut := s[:41]
		if i := strings.LastIndex(cut, "-"); i > 0 {
			s = cut[:i]
		} else {
			s = s[:40]
		}
	}
	if s == "" {
		return "node"
	}
	return s
}

func (g *Graph) resolve(q string) string {
	if _, ok := g.Nodes[q]; ok {
		return q
	}
	var hits []string
	for _, i := range g.IDs {
		if strings.HasPrefix(i, q) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		for _, i := range g.IDs {
			if strings.Contains(i, q) {
				hits = append(hits, i)
			}
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0]
	case len(hits) == 0:
		die("no node matches '%s'", q)
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	die("'%s' is ambiguous: %s", q, strings.Join(hits, ", "))
	return ""
}

// ---------------------------------------------------------------- derived graph

func (g *Graph) byCreated(ids []string) {
	sort.SliceStable(ids, func(a, b int) bool {
		ca, cb := g.Nodes[ids[a]].Created, g.Nodes[ids[b]].Created
		if ca != cb {
			return ca < cb
		}
		return ids[a] < ids[b]
	})
}

func (g *Graph) compute() {
	g.Info = map[string]*Info{}
	for _, i := range g.IDs {
		g.Info[i] = &Info{Children: []string{}, Parents: []string{}}
	}
	for _, i := range g.IDs {
		for _, b := range g.Nodes[i].Blocks {
			if _, ok := g.Nodes[b]; ok {
				g.Info[b].Children = append(g.Info[b].Children, i)
				g.Info[i].Parents = append(g.Info[i].Parents, b)
			}
		}
	}
	for _, i := range g.IDs {
		f, n := g.Info[i], g.Nodes[i]
		g.byCreated(f.Children)
		done := 0
		for _, c := range f.Children {
			if resolved(g.Nodes[c].Status) {
				done++
			}
		}
		if n.Status == "proposed" {
			f.State, f.Label = "proposed", "Proposed"
		} else if n.Status == "needs-you" { // a question for the human shows even with open points under it
			f.State, f.Label = "needs-you", "Needs you"
		} else if !resolved(n.Status) && done < len(f.Children) {
			f.State, f.Label = "waiting", fmt.Sprintf("Waiting %d/%d", done, len(f.Children))
		} else if resolved(n.Status) && len(n.Stale) > 0 {
			f.State, f.Label = "stale", "Stale"
		} else {
			f.State, f.Label = n.Status, label(n.Status)
		}
		f.Kind = "thread"
		if len(f.Parents) == 0 {
			f.Kind = "head"
		}
	}
	for _, i := range g.IDs { // unblocks = unresolved ancestors
		seen := map[string]bool{}
		stack := append([]string{}, g.Info[i].Parents...)
		for len(stack) > 0 {
			a := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[a] {
				continue
			}
			seen[a] = true
			stack = append(stack, g.Info[a].Parents...)
		}
		f := g.Info[i]
		f.Ancestors = []string{}
		for a := range seen {
			f.Ancestors = append(f.Ancestors, a)
			if !resolved(g.Nodes[a].Status) {
				f.Unblocks++
			}
		}
		sort.Strings(f.Ancestors)
	}
}

// decisionsInForce lists decisions on a node and everything above it, nearest first.
func (g *Graph) decisionsInForce(i string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	queue := []string{i}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		for _, d := range g.Nodes[c].Decisions {
			out = append(out, [2]string{c, d})
		}
		queue = append(queue, g.Info[c].Parents...)
	}
	return out
}

func (g *Graph) descendants(i string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, g.Info[i].Children...)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !seen[c] {
			seen[c] = true
			stack = append(stack, g.Info[c].Children...)
		}
	}
	return seen
}

// bfs lists a head and everything below it, level by level.
func (g *Graph) bfs(h string) []string {
	order, seen := []string{h}, map[string]bool{h: true}
	for k := 0; k < len(order); k++ {
		for _, c := range g.Info[order[k]].Children {
			if !seen[c] {
				seen[c] = true
				order = append(order, c)
			}
		}
	}
	return order
}

func (g *Graph) headsOf(i string) []string {
	out := []string{}
	for _, a := range append(append([]string{}, g.Info[i].Ancestors...), i) {
		if g.Info[a].Kind == "head" {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

func (g *Graph) heads() []string {
	var hs []string
	for _, i := range g.IDs {
		if g.Info[i].Kind == "head" {
			hs = append(hs, i)
		}
	}
	g.byCreated(hs)
	return hs
}

// actionable returns frontier nodes: needs-you, then ready, then open, by leverage.
func (g *Graph) actionable(who string) []string {
	want := map[string]map[string]bool{
		"agent": {"open": true},
		"human": {"needs-you": true, "proposed": true, "stale": true, "ready": true},
		"all":   {"open": true, "needs-you": true, "proposed": true, "stale": true, "ready": true},
	}[who]
	rank := map[string]int{"needs-you": 0, "proposed": 1, "stale": 2, "ready": 3, "open": 4}
	var ids []string
	for _, i := range g.IDs {
		if want[g.Info[i].State] {
			ids = append(ids, i)
		}
	}
	sort.SliceStable(ids, func(a, b int) bool {
		fa, fb := g.Info[ids[a]], g.Info[ids[b]]
		if rank[fa.State] != rank[fb.State] {
			return rank[fa.State] < rank[fb.State]
		}
		if fa.Unblocks != fb.Unblocks {
			return fa.Unblocks > fb.Unblocks
		}
		return g.Nodes[ids[a]].Created < g.Nodes[ids[b]].Created
	})
	return ids
}

// ---------------------------------------------------------------- text dump

// dump writes the whole graph as plain lines for the web page:
// "@ id" starts a node, then "key value" lines; lists are tab-separated;
// every body line is its own "body ..." line.
func (g *Graph) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "generated %s\n", now())
	for _, i := range g.IDs {
		n, f := g.Nodes[i], g.Info[i]
		fmt.Fprintf(&b, "@ %s\n", i)
		for _, kv := range [][2]string{
			{"title", n.Title}, {"kind", f.Kind}, {"status", n.Status}, {"state", f.State}, {"label", f.Label},
			{"mode", n.Mode}, {"agent", n.Agent}, {"outcome", n.Outcome}, {"at", n.At},
			{"created", n.Created}, {"updated", n.Updated}, {"unblocks", fmt.Sprint(f.Unblocks)},
			{"blocks", strings.Join(f.Parents, "\t")}, {"waits_on", strings.Join(f.Children, "\t")},
			{"heads", strings.Join(g.headsOf(i), "\t")}, {"touches", strings.Join(n.Touches, "\t")},
			{"stale", strings.Join(n.Stale, "\t")}, {"check", n.Check},
		} {
			fmt.Fprintf(&b, "%s %s\n", kv[0], kv[1])
		}
		for _, d := range n.Decisions {
			fmt.Fprintf(&b, "decision %s\n", d)
		}
		for _, line := range strings.Split(strings.TrimRight(n.Body, "\n"), "\n") {
			fmt.Fprintf(&b, "body %s\n", line)
		}
	}
	return b.String()
}

// nodeFileExists guards new ids against files load could not read: a new node
// must never overwrite an existing file.
func nodeFileExists(dir, id string) bool {
	_, err := os.Lstat(filepath.Join(dir, "nodes", id+".md"))
	return err == nil
}
