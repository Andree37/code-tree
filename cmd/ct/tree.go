package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const treeDir = ".tree"

var (
	statuses = []string{"open", "working", "needs-you", "ready", "done", "dropped"}
	labels   = map[string]string{
		"waiting":   "Waiting",
		"open":      "Open",
		"working":   "Working",
		"needs-you": "Needs you",
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
	return t
}

// ---------------------------------------------------------------- parse / dump

func parseNode(text, path string) *Node {
	if !strings.HasPrefix(text, "---\n") {
		die("%s: missing frontmatter", path)
	}
	end := strings.Index(text[3:], "\n---\n")
	if end < 0 {
		die("%s: unterminated frontmatter", path)
	}
	end += 3
	n := &Node{
		ID:     strings.TrimSuffix(filepath.Base(path), ".md"),
		Status: "open",
		Extra:  map[string]string{},
		Body:   text[end+5:],
	}
	for _, line := range strings.Split(text[4:end], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		str := v
		if strings.HasPrefix(v, `"`) {
			_ = json.Unmarshal([]byte(v), &str)
		}
		switch k {
		case "id":
			n.ID = str
		case "title":
			n.Title = str
		case "mode":
			n.Mode = str
		case "agent":
			n.Agent = str
		case "status":
			n.Status = str
		case "outcome":
			n.Outcome = str
		case "created":
			n.Created = str
		case "updated":
			n.Updated = str
		case "blocks":
			var list []string
			if strings.HasPrefix(v, "[") && json.Unmarshal([]byte(v), &list) == nil {
				n.Blocks = list
			} else if str != "" {
				n.Blocks = []string{str}
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
	return n
}

func jsonStr(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

func dumpNode(n *Node) string {
	vals := map[string]string{
		"id": n.ID, "title": jsonStr(n.Title), "mode": n.Mode, "agent": n.Agent, "status": n.Status,
		"blocks": jsonStr(n.Blocks), "outcome": jsonStr(n.Outcome), "created": n.Created, "updated": n.Updated,
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, k := range fieldOrder {
		fmt.Fprintf(&b, "%s: %s\n", k, vals[k])
	}
	extra := make([]string, 0, len(n.Extra))
	for k := range n.Extra {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		fmt.Fprintf(&b, "%s: %s\n", k, n.Extra[k])
	}
	b.WriteString("---\n")
	b.WriteString(n.Body)
	return b.String()
}

// ---------------------------------------------------------------- load / save

func load(dir string) *Graph {
	g := &Graph{Dir: dir, Nodes: map[string]*Node{}}
	paths, _ := filepath.Glob(filepath.Join(dir, "nodes", "*.md"))
	sort.Strings(paths)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			die("%v", err)
		}
		n := parseNode(string(data), p)
		if _, dup := g.Nodes[n.ID]; !dup {
			g.IDs = append(g.IDs, n.ID)
		}
		g.Nodes[n.ID] = n
	}
	g.compute()
	return g
}

func (g *Graph) save(n *Node) {
	n.Updated = now()
	p := filepath.Join(g.Dir, "nodes", n.ID+".md")
	if err := os.WriteFile(p, []byte(dumpNode(n)), 0o644); err != nil {
		die("%v", err)
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
		if !resolved(n.Status) && done < len(f.Children) {
			f.State, f.Label = "waiting", fmt.Sprintf("Waiting %d/%d", done, len(f.Children))
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
		"human": {"needs-you": true, "ready": true},
		"all":   {"open": true, "needs-you": true, "ready": true},
	}[who]
	rank := map[string]int{"needs-you": 0, "ready": 1, "open": 2}
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

// ---------------------------------------------------------------- JSON views

type NodeJSON struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Kind     string     `json:"kind"`
	Status   string     `json:"status"`
	State    string     `json:"state"`
	Label    string     `json:"label"`
	Mode     string     `json:"mode"`
	Agent    string     `json:"agent"`
	Outcome  string     `json:"outcome"`
	Blocks   []string   `json:"blocks"`
	WaitsOn  []string   `json:"waits_on"`
	Unblocks int        `json:"unblocks"`
	Heads    []string   `json:"heads"`
	Created  string     `json:"created"`
	Updated  string     `json:"updated"`
	Body     *string    `json:"body,omitempty"`
	Chain    []NodeJSON `json:"chain,omitempty"`
	Children []NodeJSON `json:"children,omitempty"`
}

func (g *Graph) nodeJSON(i string, full bool) NodeJSON {
	n, f := g.Nodes[i], g.Info[i]
	d := NodeJSON{
		ID: i, Title: n.Title, Kind: f.Kind, Status: n.Status, State: f.State, Label: f.Label,
		Mode: n.Mode, Agent: n.Agent, Outcome: n.Outcome, Blocks: f.Parents, WaitsOn: f.Children,
		Unblocks: f.Unblocks, Heads: g.headsOf(i), Created: n.Created, Updated: n.Updated,
	}
	if full {
		body := n.Body
		d.Body = &body
	}
	return d
}

type GraphJSON struct {
	Generated string     `json:"generated"`
	Nodes     []NodeJSON `json:"nodes"`
}

func (g *Graph) graphJSON(full bool) GraphJSON {
	out := GraphJSON{Generated: now(), Nodes: []NodeJSON{}}
	for _, i := range g.IDs {
		out.Nodes = append(out.Nodes, g.nodeJSON(i, full))
	}
	return out
}

func printJSON(v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	os.Stdout.Write(b.Bytes())
}
