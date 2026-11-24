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
		k, v, ok := strings.Cut(stripComment(sc.Text()), "=")
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

// stripComment cuts a trailing "# comment": a '#' that starts the line or
// follows whitespace. A '#' inside a value (a chat command, a path) is kept.
func stripComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (strings.TrimSpace(line[:i]) == "" || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
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

// resolveCommit turns a revision into a full commit sha, or reports that it is
// not a commit here. Revisions come from node files and .tree/sync, which
// anyone can edit, so one like "--output=x" must never reach git as an option.
func resolveCommit(cfg Config, rev string) (string, bool) {
	if rev == "" {
		return "", false
	}
	sha, err := git(cfg.Code, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || sha == "" {
		return "", false
	}
	return sha, true
}

func diffNames(cfg Config, from, to string) ([]change, error) {
	a, ok := resolveCommit(cfg, from)
	if !ok {
		return nil, fmt.Errorf("%q is not a commit in %s", from, cfg.Code)
	}
	b, ok := resolveCommit(cfg, to)
	if !ok {
		return nil, fmt.Errorf("%q is not a commit in %s", to, cfg.Code)
	}
	// --relative: paths relative to the code folder, even when it is a subfolder of the git repo.
	// -z: paths verbatim (no quoting of non-ASCII names), NUL-separated.
	out, err := git(cfg.Code, "diff", "--name-status", "-z", "-M", "--no-color", "--relative", "--end-of-options", a, b)
	if err != nil {
		return nil, err
	}
	var cs []change
	f := strings.Split(out, "\x00")
	for k := 0; k < len(f); {
		st := f[k]
		k++
		if st == "" {
			continue
		}
		c := change{Status: st[:1]}
		if (c.Status == "R" || c.Status == "C") && k+1 < len(f) { // renames: old path, then new path
			c.Old, c.Path = f[k], f[k+1]
			k += 2
		} else if k < len(f) {
			c.Path = f[k]
			k++
		}
		if c.Path != "" {
			cs = append(cs, c)
		}
	}
	return cs, nil
}

// changedSince lists paths changed from rev to HEAD plus uncommitted changes.
// A rev that is not a commit here (yet to be made, or gone) adds nothing.
func changedSince(cfg Config, rev string) []string {
	set := map[string]bool{}
	if rev != "" {
		if cs, err := diffNames(cfg, rev, "HEAD"); err == nil {
			for _, c := range cs {
				set[c.Path] = true
			}
		}
	}
	// status paths are relative to the repo root: keep those inside the code folder, relative to it.
	// --no-optional-locks: never take .git/index.lock, which would break a git command the user runs at the same time.
	prefix, _ := git(cfg.Code, "rev-parse", "--show-prefix")
	if out, err := git(cfg.Code, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all", "--", "."); err == nil && out != "" {
		f := strings.Split(out, "\x00")
		for k := 0; k < len(f); k++ {
			e := f[k]
			if len(e) < 4 {
				continue
			}
			if e[0] == 'R' || e[0] == 'C' || e[1] == 'R' || e[1] == 'C' {
				k++ // the old path of a rename follows as its own entry
			}
			if p := e[3:]; strings.HasPrefix(p, prefix) {
				set[strings.TrimPrefix(p, prefix)] = true
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

// match reports whether a repo path is covered by a node's touch pattern:
// "dir/" covers everything below dir, "a/**" likewise, "**/x" matches x at any
// depth, globs use path.Match, anything else must be the exact path. A touch
// without '/' (main.go, *.go) is a root path only.
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
		ok, _ := path.Match(pat, p)
		return ok
	default:
		return pat == p
	}
}

func matchAny(pats []string, p string) bool {
	return slices.ContainsFunc(pats, func(pat string) bool { return match(pat, p) })
}

// ignored reports whether a path matches an ignore pattern. Unlike touches, a
// bare ignore name or glob (go.sum, *.lock) matches at any depth.
func ignored(cfg Config, p string) bool {
	return slices.ContainsFunc(cfg.Ignore, func(pat string) bool {
		pat = strings.TrimPrefix(pat, "./")
		return match(pat, p) || !strings.Contains(pat, "/") && match(pat, path.Base(p))
	})
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
		delete(n.Extra, "uncommitted")
		// Its own files with uncommitted edits: the commit that brings them in is
		// its own work, not a change after it. Only those paths are recorded, so
		// edits elsewhere never hide later changes.
		var own []string
		for _, p := range changedSince(cfg, "") {
			if matchAny(n.Touches, p) && !strings.ContainsAny(p, ",\r\n") {
				own = append(own, p)
			}
		}
		if len(own) > 0 {
			n.Extra["uncommitted"] = strings.Join(own, ", ")
		}
	}
}

// uncommittedPaths is what recordCode stored; nil when there is none. The
// older "1" form meant every touched path.
func uncommittedPaths(n *Node) (paths []string, all bool) {
	v := n.Extra["uncommitted"]
	switch v {
	case "":
		return nil, false
	case "1":
		return nil, true
	}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, false
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
			if !ignored(cfg, p) {
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

	// a path that a node now owns, or that a new ignore pattern covers, is no longer unmapped
	claimed := func(p string) bool {
		return ignored(cfg, p) || slices.ContainsFunc(g.IDs, func(i string) bool { return matchAny(g.Nodes[i].Touches, p) })
	}
	if st.Head == head { // no new commits: only drop unmapped paths that are now owned or ignored
		kept := slices.DeleteFunc(slices.Clone(st.Unmapped), claimed)
		if len(kept) != len(st.Unmapped) && !dry {
			st.Unmapped = kept
			writeSync(dir, st)
			writeGraphMD(dir)
		}
		st.Unmapped = kept
		report(0)
		return
	}
	if st.Head != "" && !commitExists(cfg, st.Head) && check {
		die("last synced commit %s is not in this clone, so changes since it cannot be checked.%s", short(st.Head), shallowHint(cfg))
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
	// commit count; cache one diff per distinct resolution commit. A commit
	// that is no longer here (rebased away, or cut off by a shallow clone)
	// cannot rule a change out, so every change counts.
	since := map[string]map[string]bool{}
	gone := map[string]bool{}
	changedAfter := func(n *Node, p string) bool {
		if n.At == "" || n.At == st.Head {
			return true
		}
		if isGone, seen := gone[n.At]; seen && isGone {
			return true
		} else if !seen {
			gone[n.At] = !commitExists(cfg, n.At)
			if gone[n.At] {
				fmt.Fprintf(os.Stderr, "ct: %s was resolved at %s, which is not in this repo (rebased away?); every later change to its files counts.%s\n", n.ID, short(n.At), shallowHint(cfg))
				return true
			}
		}
		if isAncestor(cfg, n.At, st.Head) {
			return true
		}
		set, ok := since[n.At]
		if !ok {
			cs, err := diffNames(cfg, n.At, head)
			if err != nil {
				die("%v", err)
			}
			set = map[string]bool{}
			for _, c := range cs {
				set[c.Path] = true
				if c.Old != "" {
					set[c.Old] = true
				}
			}
			since[n.At] = set
		}
		return set[p]
	}

	prevUnmapped := strings.Join(st.Unmapped, "\n")
	touched := map[string]*Node{}
	ownCommitted := map[string]bool{}
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
			if c.Status == "D" {
				delete(unmapped, c.Path) // a deleted file needs no owner any more
			} else if !ignored(cfg, c.Path) {
				unmapped[c.Path] = true
			}
			continue
		}
		delete(unmapped, c.Path)
		for _, i := range ids {
			n := g.Nodes[i]
			// only done work goes stale: dropped, rejected and merged nodes are never picked up again
			if n.Status != "done" || !changedAfter(n, c.Path) && !(c.Old != "" && changedAfter(n, c.Old)) {
				continue
			}
			// resolved before its work was committed: the commit bringing in those files is its own
			if own, all := uncommittedPaths(n); all || slices.Contains(own, c.Path) || c.Old != "" && slices.Contains(own, c.Old) {
				ownCommitted[i] = true
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

	for i := range ownCommitted {
		n := g.Nodes[i]
		delete(n.Extra, "uncommitted")
		if len(n.Stale) == 0 { // nothing else changed after it: it is current as of this commit
			n.At = head
		}
		touched[i] = n
		logs[i] = append(logs[i], "its own work was committed")
	}

	// files that a node claims now (touches added since), or that a new ignore pattern covers, are no longer unmapped
	for p := range unmapped {
		if claimed(p) {
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

func commitExists(cfg Config, rev string) bool {
	_, ok := resolveCommit(cfg, rev)
	return ok
}

// isAncestor reports whether a is an ancestor of b; both must be commits here.
func isAncestor(cfg Config, a, b string) bool {
	x, ok1 := resolveCommit(cfg, a)
	y, ok2 := resolveCommit(cfg, b)
	if !ok1 || !ok2 {
		return false
	}
	_, err := git(cfg.Code, "merge-base", "--is-ancestor", x, y)
	return err == nil
}

func shallowHint(cfg Config) string {
	if s, _ := git(cfg.Code, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(s) == "true" {
		return " This clone is shallow: fetch full history (e.g. actions/checkout with fetch-depth: 0)."
	}
	return ""
}
