package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stray files in nodes/ (editor locks, macOS ._ files, notes) are skipped.
func TestStrayFilesDoNotBreakCT(t *testing.T) {
	dir := newTree(t)
	mustCT(t, dir, nil, "add", "Goal")
	nodes := filepath.Join(dir, treeDir, "nodes")
	_ = os.Symlink("user@host.123", filepath.Join(nodes, ".#goal.md")) // emacs lock, dangling
	_ = os.WriteFile(filepath.Join(nodes, "._goal.md"), []byte{0, 5, 22, 7}, 0o644)
	_ = os.WriteFile(filepath.Join(nodes, "README.md"), []byte("notes\n"), 0o644)
	out, ok := ct(t, dir, nil, "ls")
	if !ok || !strings.Contains(out, "Goal") {
		t.Fatalf("ct ls broke on stray files: ok=%v %s", ok, out)
	}
}

// A project folder named with glob characters keeps all its nodes.
func TestGlobCharactersInPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj [v2]")
	_ = os.MkdirAll(dir, 0o755)
	mustCT(t, dir, nil, "init")
	first := mustCT(t, dir, nil, "add", "Goal", "-d", "first")
	second := mustCT(t, dir, nil, "add", "Goal", "-d", "second")
	if first == second {
		t.Fatalf("second add reused id %q and overwrote the first node", first)
	}
	if out := mustCT(t, dir, nil, "ls"); strings.Count(out, "Goal") != 2 {
		t.Fatalf("want both nodes listed, got: %s", out)
	}
}

// Merging a node into its own child would make a loop: refused, and show never hangs.
func TestMergeRefusesLoops(t *testing.T) {
	dir := newTree(t)
	goal := mustCT(t, dir, nil, "add", "Goal")
	parent := mustCT(t, dir, nil, "add", "Parent", "-b", goal)
	kid := mustCT(t, dir, nil, "add", "Kid", "-b", parent)
	if out, ok := ct(t, dir, nil, "merge", parent, kid); ok {
		t.Fatalf("merge into a child was allowed: %s", out)
	}
	done := make(chan bool)
	go func() { ct(t, dir, nil, "show", kid); done <- true }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ct show hung")
	}
}

// What waited on a merged node keeps waiting, now on the node it was merged into.
func TestMergeKeepsParentsWaiting(t *testing.T) {
	dir := newTree(t)
	ship := mustCT(t, dir, nil, "add", "Ship release")
	auth := mustCT(t, dir, nil, "add", "Auth rework")
	login := mustCT(t, dir, nil, "add", "Login fix in auth", "-b", auth)
	dup := mustCT(t, dir, agentEnv, "add", "Fix login bug", "-b", ship)
	mustCT(t, dir, nil, "merge", dup, login)
	if out, ok := ct(t, dir, nil, "done", ship); ok {
		t.Fatalf("ship-release could be finished while the merged work is open: %s", out)
	}
}

// A decision made while a check runs is not lost when the check finishes.
func TestDecisionSurvivesSlowCheck(t *testing.T) {
	dir := newTree(t)
	goal := mustCT(t, dir, nil, "add", "Goal")
	node := mustCT(t, dir, nil, "add", "Work", "-b", goal, "--check", "sleep 2")
	mustCT(t, dir, nil, "claim", node, "--by", "agent1")

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), "CT_TEST_MAIN=1", "CT_TEST_ARGV="+strings.Join([]string{"ready", node, "-o", "x", "--by", "agent1"}, "\t"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	mustCT(t, dir, nil, "decide", node, "no allocations")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ready failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, treeDir, "nodes", node+".md"))
	if !strings.Contains(string(data), "decision: no allocations") {
		t.Fatalf("decision lost:\n%s", data)
	}
	if !strings.Contains(string(data), "status: ready") {
		t.Fatalf("ready not applied:\n%s", data)
	}
}

// ct see on an empty folder must not crash on enter, t, g, s or x.
func TestSeeEmptyDoesNotCrash(t *testing.T) {
	dir := newTree(t)
	s := &seer{tree: filepath.Join(dir, treeDir), st: convoState{}, results: make(chan string, 4)}
	s.g = load(s.tree)
	s.layout()
	for _, k := range []string{"\r", "t", "g", "s", "x", "\x1b[A", "\x1b[C"} {
		s.key(k)
	}
}
