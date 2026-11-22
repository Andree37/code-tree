package main

// Keeping the tree in sync with a code repo, without touching that repo.
//
// ct only ever runs read-only git commands against the code (rev-parse, diff,
// merge-base, cat-file). Nodes say which paths they own via `touches`; a sync
// diffs the code from the last synced commit to HEAD and only rewrites nodes
// whose paths changed.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- config

type Config struct {
	Code     string   // absolute path of the code repo
	Raw      string   // code path as written in the config
	Ignore   []string // patterns never reported as unmapped
	Approve  string   // "agents": nodes added by agents start proposed; "none": no approval step
	Chat     string   // agent command for chats: prompt on stdin, reply on stdout
	ChatName string   // how that agent appears in logs
}

const defaultConfig = `# code-tree config
# code: the repo this tree describes, relative to the folder holding .tree/.
#       It is only read (git rev-parse / diff), never written to.
code = .
# ignore: comma-separated patterns that never count as unmapped changes.
ignore = go.sum, *.lock, package-lock.json
# approve: agents = nodes an agent adds start as Proposed until you accept them
#          none   = agents add open nodes directly
approve = agents
# chat: agent command for node chats (prompt on stdin, reply on stdout), run in the code repo
chat = claude -p --allowedTools "Read,Grep,Glob,Bash(ct:*)"
chat_name = claude
`

func loadConfig(tree string) Config {
	base := filepath.Dir(tree)
	cfg := Config{Code: base, Raw: ".", Approve: "agents", Chat: defaultChat, ChatName: "claude"}
	f, err := os.Open(filepath.Join(tree, "config"))
	if err != nil {
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "code":
			cfg.Raw = v
			if !filepath.IsAbs(v) {
				v = filepath.Join(base, v)
			}
			cfg.Code = filepath.Clean(v)
		case "approve":
			cfg.Approve = v
		case "chat":
			cfg.Chat = v
		case "chat_name":
			cfg.ChatName = v
		case "ignore":
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					cfg.Ignore = append(cfg.Ignore, p)
				}
			}
		}
	}
	// a tree that lives inside the code repo never reports itself
	if rel, err := filepath.Rel(cfg.Code, tree); err == nil && !strings.HasPrefix(rel, "..") {
		cfg.Ignore = append(cfg.Ignore, filepath.ToSlash(rel)+"/")
	}
	return cfg
}

// ---------------------------------------------------------------- git (read-only)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func codeHead(cfg Config) string {
	h, err := git(cfg.Code, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return h
}

type change struct {
	Status string // A M D R T C
	Path   string
	Old    string // for renames
}

func diffNames(cfg Config, from, to string) ([]change, error) {
	out, err := git(cfg.Code, "diff", "--name-status", "-M", "--no-color", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	var cs []change
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		c := change{Status: f[0][:1], Path: f[len(f)-1]}
		if (c.Status == "R" || c.Status == "C") && len(f) == 3 {
			c.Old = f[1]
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// changedSince lists paths changed from rev to HEAD plus uncommitted changes.
func changedSince(cfg Config, rev string) []string {
	set := map[string]bool{}
	if rev != "" {
		if cs, err := diffNames(cfg, rev, "HEAD"); err == nil {
			for _, c := range cs {
				set[c.Path] = true
			}
		}
	}
	if out, err := git(cfg.Code, "status", "--porcelain", "--untracked-files=all"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 3 {
				p := line[3:]
				if _, after, ok := strings.Cut(p, " -> "); ok {
					p = after
				}
				set[strings.Trim(p, `"`)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- path patterns

// match reports whether a repo path is covered by a pattern:
// "dir/" covers everything below dir, "a/**" likewise, globs use path.Match,
// anything else must be the exact path.
func match(pat, p string) bool {
	pat = strings.TrimPrefix(pat, "./")
	switch {
	case strings.HasSuffix(pat, "/"):
		return strings.HasPrefix(p, pat)
	case strings.HasSuffix(pat, "/**"):
		return strings.HasPrefix(p, strings.TrimSuffix(pat, "**"))
	case strings.HasPrefix(pat, "**/"):
		rest := strings.TrimPrefix(pat, "**/")
		for s := p; ; {
			if ok, _ := path.Match(rest, s); ok {
				return true
			}
			_, after, found := strings.Cut(s, "/")
			if !found {
				return false
			}
			s = after
		}
	case strings.ContainsAny(pat, "*?["):
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
		if !strings.Contains(pat, "/") { // bare "*.lock" matches at any depth
			ok, _ := path.Match(pat, path.Base(p))
			return ok
		}
		return false
	default:
		return pat == p
	}
}

func matchAny(pats []string, p string) bool {
	return slices.ContainsFunc(pats, func(pat string) bool { return match(pat, p) })
}

// ---------------------------------------------------------------- sync state

// SyncState lives in .tree/sync as plain lines:
//
//	head <sha>
//	synced <time>
//	unmapped <path>   (one line per path)
type SyncState struct {
	Head, Synced string
	Unmapped     []string
}

func readSync(tree string) SyncState {
	s := SyncState{Unmapped: []string{}}
	data, err := os.ReadFile(filepath.Join(tree, "sync"))
	if err != nil {
		return s
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "head":
			s.Head = v
		case "synced":
			s.Synced = v
		case "unmapped":
			s.Unmapped = append(s.Unmapped, v)
		}
	}
	return s
}

func writeSync(tree string, s SyncState) {
	var b strings.Builder
	fmt.Fprintf(&b, "head %s\nsynced %s\n", s.Head, s.Synced)
	for _, p := range s.Unmapped {
		fmt.Fprintf(&b, "unmapped %s\n", p)
	}
	if err := writeAtomic(filepath.Join(tree, "sync"), []byte(b.String())); err != nil {
		die("%v", err)
	}
}

// ---------------------------------------------------------------- resolution hook

// recordCode stamps a node with the commit it was resolved at and clears stale
// marks; with touches "auto" it fills touches from what changed since claim.
func recordCode(g *Graph, n *Node, touches []string) {
	cfg := loadConfig(g.Dir)
	head := codeHead(cfg)
	if head == "" {
		return
	}
	recordCodeTouches(g, n, touches)
	if resolved(n.Status) {
		n.At = head
		n.Stale = nil
	}
}

func addTouches(n *Node, ts []string) {
	for _, t := range ts {
		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(n.Touches, p) {
				n.Touches = append(n.Touches, p)
			}
		}
	}
	sort.Strings(n.Touches)
}

// ---------------------------------------------------------------- commands

func cmdTouch(a *Args) {
	need(a, 2, "touch")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if a.Bool("rm") {
		n.Touches = slices.DeleteFunc(n.Touches, func(t string) bool { return slices.Contains(a.Pos[1:], t) })
	} else {
		recordCodeTouches(g, n, a.Pos[1:])
	}
	g.mutate(n)
	fmt.Printf("%s touches: %s\n", i, orDash(n.Touches))
}

func recordCodeTouches(g *Graph, n *Node, ts []string) {
	if slices.Contains(ts, "auto") {
		cfg := loadConfig(g.Dir)
		var auto []string
		for _, p := range changedSince(cfg, n.Base) {
			if !matchAny(cfg.Ignore, p) {
				auto = append(auto, p)
			}
		}
		ts = append(slices.DeleteFunc(slices.Clone(ts), func(t string) bool { return t == "auto" }), auto...)
	}
	addTouches(n, ts)
}

func cmdAck(a *Args) {
	need(a, 1, "ack")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if len(n.Stale) == 0 {
		fmt.Printf("%s is not stale\n", i)
		return
	}
	msg := "rechecked: still holds after changes to " + strings.Join(n.Stale, ", ")
	if note := a.Get("note", ""); note != "" {
		msg += ": " + note
	}
	if o := a.Get("outcome", ""); o != "" {
		n.Outcome = oneLine(o)
	}
	n.Stale = nil
	if h := codeHead(loadConfig(g.Dir)); h != "" {
		n.At = h
	}
	appendLog(n, msg, who(a))
	g.mutate(n)
	fmt.Printf("%s: %s\n", i, label(n.Status))
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func cmdSync(a *Args) {
	dir := requireTree()
	g := load(dir)
	cfg := loadConfig(dir)
	dry, check := a.Bool("dry-run"), a.Bool("check")
	st := readSync(dir)

	head := codeHead(cfg)
	if head == "" {
		die("%s is not a git repo with commits (set `code =` in %s)", cfg.Code, filepath.Join(dir, "config"))
	}

	report := func(changedNodes int) {
		if a.Bool("run-checks") && !dry {
			if ch := runStaleChecks(g, head); len(ch) > 0 {
				for _, n := range ch {
					g.save(n)
				}
				writeGraphMD(dir)
			}
		}
		stale := 0
		for _, i := range g.IDs {
			if len(g.Nodes[i].Stale) > 0 {
				stale++
			}
		}
		fmt.Printf("synced to %s · %d node(s) updated · %d stale · %d unmapped\n", short(head), changedNodes, stale, len(st.Unmapped))
		for _, i := range g.IDs {
			if s := g.Nodes[i].Stale; len(s) > 0 {
				fmt.Printf("  stale    %s  (%s)\n", i, strings.Join(s, ", "))
			}
		}
		for k, p := range st.Unmapped {
			if k == 20 {
				fmt.Printf("  … %d more unmapped (see .tree/sync)\n", len(st.Unmapped)-20)
				break
			}
			fmt.Printf("  unmapped %s\n", p)
		}
		if check && (stale > 0 || len(st.Unmapped) > 0) {
			os.Exit(1)
		}
	}

	if st.Head == head { // no new commits: only drop unmapped paths a node now owns
		kept := slices.DeleteFunc(slices.Clone(st.Unmapped), func(p string) bool {
			return slices.ContainsFunc(g.IDs, func(i string) bool { return matchAny(g.Nodes[i].Touches, p) })
		})
		if len(kept) != len(st.Unmapped) && !dry {
			st.Unmapped = kept
			writeSync(dir, st)
			writeGraphMD(dir)
		}
		st.Unmapped = kept
		report(0)
		return
	}
	if st.Head == "" || !commitExists(cfg, st.Head) {
		if st.Head != "" {
			fmt.Printf("last synced commit %s is gone (rebased?); taking %s as the new baseline\n", short(st.Head), short(head))
		} else {
			fmt.Printf("first sync: baseline is %s in %s\n", short(head), cfg.Code)
		}
		var changed []*Node
		for _, i := range g.IDs {
			if n := g.Nodes[i]; resolved(n.Status) && n.At == "" {
				n.At = head
				changed = append(changed, n)
			}
		}
		st.Head, st.Synced = head, now()
		if !dry {
			writeSync(dir, st)
			g.mutate(changed...)
		}
		report(len(changed))
		return
	}

	changes, err := diffNames(cfg, st.Head, head)
	if err != nil {
		die("%v", err)
	}

	// index: which nodes own which patterns
	owners := func(p string) []string {
		var ids []string
		for _, i := range g.IDs {
			if matchAny(g.Nodes[i].Touches, p) {
				ids = append(ids, i)
			}
		}
		return ids
	}

	// For nodes resolved after the last sync, only changes after their own
	// commit count; cache one diff per distinct resolution commit.
	since := map[string]map[string]bool{}
	changedAfter := func(n *Node, p string) bool {
		if n.At == "" || n.At == st.Head || isAncestor(cfg, n.At, st.Head) {
			return true
		}
		set, ok := since[n.At]
		if !ok {
			set = map[string]bool{}
			if cs, err := diffNames(cfg, n.At, head); err == nil {
				for _, c := range cs {
					set[c.Path] = true
					if c.Old != "" {
						set[c.Old] = true
					}
				}
			}
			since[n.At] = set
		}
		return set[p]
	}

	prevUnmapped := strings.Join(st.Unmapped, "\n")
	touched := map[string]*Node{}
	logs := map[string][]string{}
	unmapped := map[string]bool{}
	for _, p := range st.Unmapped {
		unmapped[p] = true
	}

	for _, c := range changes {
		ids := owners(c.Path)
		if c.Old != "" { // rename: move exact path entries over
			for _, i := range owners(c.Old) {
				n := g.Nodes[i]
				if k := slices.Index(n.Touches, c.Old); k >= 0 {
					n.Touches[k] = c.Path
					sort.Strings(n.Touches)
					touched[i] = n
					logs[i] = append(logs[i], fmt.Sprintf("renamed %s → %s", c.Old, c.Path))
				}
				if !slices.Contains(ids, i) {
					ids = append(ids, i)
				}
			}
			delete(unmapped, c.Old)
		}
		if len(ids) == 0 {
			if c.Status != "D" && !matchAny(cfg.Ignore, c.Path) {
				unmapped[c.Path] = true
			}
			continue
		}
		delete(unmapped, c.Path)
		for _, i := range ids {
			n := g.Nodes[i]
			if !resolved(n.Status) || !changedAfter(n, c.Path) && !(c.Old != "" && changedAfter(n, c.Old)) {
				continue
			}
			what := c.Path
			if c.Status == "D" {
				what += " (deleted)"
			}
			if !slices.Contains(n.Stale, what) {
				n.Stale = append(n.Stale, what)
				touched[i] = n
				logs[i] = append(logs[i], "code changed: "+what)
			}
		}
	}

	// files that a node claims now (touches added since) are no longer unmapped
	for p := range unmapped {
		if len(owners(p)) > 0 {
			delete(unmapped, p)
		}
	}
	st.Unmapped = make([]string, 0, len(unmapped))
	for p := range unmapped {
		st.Unmapped = append(st.Unmapped, p)
	}
	sort.Strings(st.Unmapped)

	var changed []*Node
	for _, i := range g.IDs {
		if n, ok := touched[i]; ok {
			sort.Strings(n.Stale)
			appendLog(n, fmt.Sprintf("sync %s..%s: %s", short(st.Head), short(head), strings.Join(logs[i], "; ")), "ct sync")
			changed = append(changed, n)
		}
	}
	fmt.Printf("%d file(s) changed in %s..%s\n", len(changes), short(st.Head), short(head))
	st.Head, st.Synced = head, now()
	if !dry {
		writeSync(dir, st)
		for _, n := range changed {
			g.save(n)
		}
		if len(changed) > 0 || strings.Join(st.Unmapped, "\n") != prevUnmapped {
			writeGraphMD(dir)
		}
	}
	report(len(changed))
}

func commitExists(cfg Config, sha string) bool {
	_, err := git(cfg.Code, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func isAncestor(cfg Config, a, b string) bool {
	_, err := git(cfg.Code, "merge-base", "--is-ancestor", a, b)
	return err == nil
}
