package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStdout runs f and returns what it printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// selfEnv lets runSelf (which starts os.Executable, this test binary) act as ct.
func selfEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("CT_TEST_MAIN", "1")
	t.Setenv("CT_HOME", filepath.Join(dir, ".cthome"))
	for _, k := range []string{"CT_AGENT", "CLAUDECODE", "IC_ID", "CT_TREE", "HERDR_TAB_ID", "HERDR_PANE_ID"} {
		t.Setenv(k, "")
	}
}

// Only the pane's own colour codes reach the terminal.
func TestTermSafeKeepsColoursDropsEscapes(t *testing.T) {
	in := cBold + "title" + cReset + " mode \x1b]52;c;cm0gLXJmIH4K\x07 x\x1b[2J\x9b31m\r\n"
	got := termSafe(in)
	if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x07") || strings.Contains(got, "\x1b[2J") || strings.ContainsRune(got, 0x9b) {
		t.Fatalf("escape left in %q", got)
	}
	if !strings.Contains(got, cBold+"title"+cReset) {
		t.Fatalf("pane colours lost: %q", got)
	}
}

// An agent's --mode, touches and id never reach the terminal raw through ct watch.
func TestWatchDrawsNoAgentEscapes(t *testing.T) {
	dir := newTree(t)
	osc := "\x1b]52;c;cm0gLXJmIH4K\x07"
	// The CLI refuses control characters, so plant them the way an agent
	// writing the file directly would.
	id := mustCT(t, dir, agentEnv, "add", "Refactor", "-m", "build", "-t", "a.go")
	file := filepath.Join(dir, treeDir, "nodes", id+".md")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "mode: build", "mode: build"+osc, 1)
	text = strings.Replace(text, "touches: a.go", "touches: a"+osc+".go", 1)
	if text == string(data) {
		t.Fatalf("could not plant the escapes in:\n%s", data)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &watcher{tree: filepath.Join(dir, treeDir), view: "next", results: make(chan string, 4), w: 100, h: 30}
	w.reload()
	w.sel = id
	out := captureStdout(t, func() {
		w.render()
		w.key([]byte("\r")) // the full node: mode, touches, check
		w.render()
	})
	if strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x07") {
		t.Fatalf("OSC 52 reached the terminal: %q", out)
	}
}

// ct see: tiny panes and the folder list never panic.
func TestSeeTinyPaneWithPicker(t *testing.T) {
	dir := newTree(t)
	t.Setenv("CT_HOME", filepath.Join(dir, ".cthome"))
	s := &seer{tree: filepath.Join(dir, treeDir), st: convoState{}, results: make(chan string, 4)}
	s.g = load(s.tree)
	for h := 1; h <= 9; h++ {
		s.h, s.w = h, 60
		s.pickOn, s.pickRows, s.pickSel = true, nil, 0
		captureStdout(t, func() { s.render() })
		s.key("\x1b[B")
		s.key("\r") // enter on an empty list
	}
}

// A status message is drawn before the long help line, so a narrow pane shows it.
func TestSeeMessageNotHiddenByHelp(t *testing.T) {
	dir := newTree(t)
	s := &seer{tree: filepath.Join(dir, treeDir), st: convoState{}, results: make(chan string, 4), w: 60, h: 20}
	s.g = load(s.tree)
	s.msg = "sent to claude"
	out := captureStdout(t, func() { s.render() })
	if !strings.Contains(out, "sent to claude") {
		t.Fatalf("message cut off: %q", out)
	}
}

// Wide characters take two columns: cut never makes a line wider than asked.
func TestCutCountsWideCharacters(t *testing.T) {
	for _, s := range []string{"数据库迁移计划和回滚步骤", "🎉🎉🎉🎉🎉🎉🎉🎉", "café au lait", "mixed 数据 🎉 text"} {
		for n := 1; n <= 12; n++ {
			if got := cut(s, n); visible(got) > n {
				t.Fatalf("cut(%q, %d) = %q is %d wide", s, n, got, visible(got))
			}
		}
	}
	if visible("数据") != 4 || visible("ab") != 2 || visible("é") != 1 {
		t.Fatal("widths wrong")
	}
}

// A burst pasted without bracketed paste (several characters and a line break
// in one read) is a paste, not hotkeys: with no prompt open, nothing happens.
func TestUnbracketedPasteIsNotHotkeys(t *testing.T) {
	s := &seer{results: make(chan string, 4), st: convoState{}}
	in := newPaneInput()
	if !drive(s, in, "drop it\nquit now\n") {
		t.Fatal("a pasted q closed the pane")
	}
	if !strings.Contains(s.msg, "paste ignored") {
		t.Fatalf("msg = %q", s.msg)
	}
	s, sent := promptPane()
	in = newPaneInput()
	drive(s, in, "use postgres\nfor the jobs table", "\r")
	if len(*sent) != 1 || (*sent)[0] != "use postgres for the jobs table" {
		t.Fatalf("sent %q", *sent)
	}
}

// The graph pane's tag routes to the right box even when the folder name has "]" or "·".
func TestPointerHandlesOddFolderNames(t *testing.T) {
	dir := newTree(t)
	g := load(filepath.Join(dir, treeDir))
	id := mustCT(t, dir, nil, "add", "Goal")
	kid := mustCT(t, dir, nil, "add", "LRU cache", "-b", id)
	g = load(filepath.Join(dir, treeDir))
	for _, pkg := range []string{"src", "we]rd", "a · b"} {
		for _, msg := range []string{
			"[code-tree · " + pkg + " · " + kid + " · LRU cache]\nwhy?",
			"[code-tree · " + pkg + " · " + kid + "] Let's go back to the thread",
		} {
			if got := pointedAt(msg, g); got != kid {
				t.Fatalf("pointedAt(%q) = %q, want %q", msg, got, kid)
			}
		}
	}
}

// The tag's package decides which package's tree a turn lands in.
func TestTaggedPackageRoutesToItsTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CT_HOME", home)
	root := filepath.Join(t.TempDir(), "repo")
	pkgTree(root, "web]app")
	pkgTree(root, "api")
	if rel, ok := taggedPackage("[code-tree · web]app · fix-login · Fix login]\nhi", root); !ok || rel != "web]app" {
		t.Fatalf("taggedPackage = %q %v", rel, ok)
	}
	if _, ok := taggedPackage("[code-tree · nowhere · fix-login]", root); ok {
		t.Fatal("unknown package matched")
	}
}

// Inside herdr, ct see follows only its own tab's Claude, even after it ended.
func TestSeeFollowsOnlyItsOwnTab(t *testing.T) {
	ic := t.TempDir()
	t.Setenv("IC_HOME", ic)
	t.Setenv("CT_HOME", t.TempDir())
	_ = os.MkdirAll(filepath.Join(ic, "sessions"), 0o755)
	write := func(id, tab, ended, seen string) {
		body := "id " + id + "\nkind claude\ntab " + tab + "\nseen " + seen + "\n"
		if ended != "" {
			body += "ended " + ended + "\n"
		}
		_ = os.WriteFile(filepath.Join(ic, "sessions", id), []byte(body), 0o644)
	}
	write("mine", "t1", "2026-11-27T10:00:00Z", "2026-11-27T09:00:00Z")
	write("other", "t2", "", "2026-11-27T11:00:00Z")
	t.Setenv("HERDR_TAB_ID", "t1")
	s := &seer{session: "mine", auto: true}
	id, ended := s.follow()
	if id != "mine" || !ended {
		t.Fatalf("follow = %q ended=%v, want mine (ended)", id, ended)
	}
	s.ended = ended
	s.send("hello")
	if !strings.Contains(s.msg, "ended") {
		t.Fatalf("send to an ended Claude: msg %q", s.msg)
	}
	write("new", "t1", "", "2026-11-27T12:00:00Z") // /clear or a new Claude in this tab
	if id, ended := s.follow(); id != "new" || ended {
		t.Fatalf("follow = %q ended=%v, want new", id, ended)
	}
}

// x then x again puts back the question the node was waiting on.
func TestSeeUndoKeepsQuestion(t *testing.T) {
	dir := newTree(t)
	t.Setenv("CT_HOME", filepath.Join(dir, ".cthome"))
	goal := mustCT(t, dir, nil, "add", "Goal")
	id := mustCT(t, dir, nil, "add", "Pick a DB", "-b", goal)
	mustCT(t, dir, nil, "ask", id, "Postgres or SQLite?")
	tree := filepath.Join(dir, treeDir)
	before := load(tree).Nodes[id].Outcome
	if load(tree).Nodes[id].Status != "needs-you" {
		t.Fatal("setup: not needs-you")
	}
	s := &seer{tree: tree, sel: id, st: convoState{}, results: make(chan string, 4)}
	s.toggleWaiting()
	s.tree, s.sel = tree, id // reload follows the (empty) session; point back
	s.toggleWaiting()
	n := load(tree).Nodes[id]
	if n.Status != "needs-you" || n.Outcome != before {
		t.Fatalf("after undo: status %q outcome %q, want needs-you %q", n.Status, n.Outcome, before)
	}
}

// Page and pane actions name whole ids: a prefix never lands on another node,
// and a relative CT_TREE still works from the child's folder.
func TestPaneActionsUseExactIDsAndAbsoluteTree(t *testing.T) {
	dir := newTree(t)
	selfEnv(t, dir)
	id := mustCT(t, dir, nil, "add", "Goal number one")
	tree := filepath.Join(dir, treeDir)
	if out, err := runSelf(tree, []string{"note", "--by", "human", "--", id[:4], "hi"}); err == nil {
		t.Fatalf("prefix %q acted on %s: %s", id[:4], id, out)
	}
	t.Chdir(dir)
	if out, err := runSelf(treeDir, []string{"note", "--by", "human", "--", id, "hello there"}); err != nil {
		t.Fatalf("relative tree: %v %s", err, out)
	}
	if !strings.Contains(load(tree).Nodes[id].Body, "hello there") {
		t.Fatal("note not written")
	}
}

// y on a node whose check is slow never freezes ct watch.
func TestWatchActDoesNotBlock(t *testing.T) {
	dir := newTree(t)
	selfEnv(t, dir)
	goal := mustCT(t, dir, nil, "add", "Goal")
	id := mustCT(t, dir, nil, "add", "Work", "-b", goal, "--check", "sleep 2")
	w := &watcher{tree: filepath.Join(dir, treeDir), view: "next", results: make(chan string, 4)}
	w.reload()
	start := time.Now()
	w.act("done", id, "")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("act blocked the pane for %v", d)
	}
	select {
	case <-w.results:
	case <-time.After(20 * time.Second):
		t.Fatal("no result")
	}
	waitChildren(5 * time.Second)
}
