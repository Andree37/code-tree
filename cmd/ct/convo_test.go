package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func convoTree(t *testing.T) string {
	t.Helper()
	tree := filepath.Join(t.TempDir(), treeDir)
	if err := os.MkdirAll(filepath.Join(tree, "nodes"), 0o755); err != nil {
		t.Fatal(err)
	}
	return tree
}

func status(t *testing.T, tree, id string) string {
	t.Helper()
	n := load(tree).Nodes[id]
	if n == nil {
		t.Fatalf("no node %s", id)
	}
	return n.Status
}

// A point is only settled-and-counted when nothing under it is still open.
func TestSettledPointWithOpenChildKeepsCounting(t *testing.T) {
	tree := convoTree(t)
	thread := apply(tree, "c", placement{thread: "new", parent: "new-topic", topic: "Subject", title: "Thread T", status: "open"})
	sub := apply(tree, "c", placement{thread: "new", parent: thread, title: "Question S", status: "needs-you", summary: "A or B?"})
	apply(tree, "c", placement{thread: thread, status: "ready", summary: "decided"})
	if got := status(t, tree, thread); got == "done" {
		t.Fatal("T counted as settled while S under it still waits for you")
	}
	g := load(tree)
	if g.Info[sub].State != "needs-you" {
		t.Fatalf("S should still wait for you, is %s", g.Info[sub].State)
	}
	// once S is answered, T counts as settled and so does the subject
	apply(tree, "c", placement{thread: sub, status: "ready", summary: "chose A"})
	if got := status(t, tree, thread); got != "done" {
		t.Fatalf("T = %s, want done once everything under it settled", got)
	}
	g = load(tree)
	head := g.heads()[0]
	if g.Info[head].State != "done" || plainLabel(g.Info[head]) != "All settled" {
		t.Fatalf("subject = %s / %q, want settled", g.Info[head].State, plainLabel(g.Info[head]))
	}
}

// A new point under a settled one reopens it.
func TestNewPointReopensSettledParent(t *testing.T) {
	tree := convoTree(t)
	thread := apply(tree, "c", placement{thread: "new", parent: "new-topic", topic: "Subject", title: "Thread T", status: "ready"})
	if status(t, tree, thread) != "done" {
		t.Fatal("setup: T should be settled")
	}
	apply(tree, "c", placement{thread: "new", parent: thread, title: "Follow-up", status: "open"})
	if got := status(t, tree, thread); got == "done" || got == "ready" {
		t.Fatalf("T stayed settled (%s) with a new open point under it", got)
	}
}

// Settling a waiting-for-you point never keeps the unanswered question as its text.
func TestAnsweredQuestionIsNotTheOutcome(t *testing.T) {
	tree := convoTree(t)
	q := apply(tree, "c", placement{thread: "new", parent: "new-topic", topic: "S", title: "Old graph", status: "needs-you", summary: "Should I delete the old graph?"})
	apply(tree, "c", placement{thread: "new", parent: "new-topic", topic: "S2", title: "Other", status: "open", summary: "deleted it", close: []string{q}})
	if out := load(tree).Nodes[q].Outcome; strings.Contains(out, "Should I delete") {
		t.Fatalf("settled box still shows the question: %q", out)
	}
}

// The model sometimes answers with the status it was shown, or adds stray lines.
func TestParsePlacementIsForgiving(t *testing.T) {
	p := parsePlacement("thread: map-keys\nstatus: done\nsummary: `keys are comptime-only`\nStatus: Postgres is already running\nclose: `a`, b")
	if p.status != "ready" {
		t.Errorf("status done read as %q, want ready", p.status)
	}
	if p.summary != "keys are comptime-only" {
		t.Errorf("summary = %q", p.summary)
	}
	if strings.Join(p.close, ",") != "a,b" {
		t.Errorf("close = %q", p.close)
	}
	if q := parsePlacement("summary: p` opens side list"); q.summary != "p` opens side list" {
		t.Errorf("inner backtick stripped: %q", q.summary)
	}
}

// Only what the human typed becomes a turn; intercom messages count as theirs.
func TestReadTurnsKeepsOnlyTheHuman(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"add an LRU cache"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"file_path":"/r/store/lru.go"}}]}}`,
		`{"type":"user","isCompactSummary":true,"message":{"content":"This session is being continued from a previous conversation..."}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done: the LRU cache is in store/lru.go"}]}}`,
		`{"type":"user","message":{"content":"/code-review max"}}`,
		`{"type":"user","message":{"content":"<task-notification>done</task-notification>"}}`,
		`{"type":"user","message":{"content":"[intercom] New message from human."}}`,
		`{"type":"attachment","attachment":{"hookEvent":"UserPromptSubmit","content":"[intercom] 1 new message(s)\n[code-tree · src · lru-cache · LRU cache]\nwhy LRU?"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Because..."}]}}`,
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	turns := readTurns(path)
	if len(turns) != 2 {
		for _, x := range turns {
			t.Logf("turn: %q -> %q", x.prompt, x.reply)
		}
		t.Fatalf("got %d turns, want 2 (the LRU request and the intercom message)", len(turns))
	}
	if !strings.Contains(turns[0].reply, "LRU cache is in store/lru.go") {
		t.Errorf("the real turn lost its reply: %q", turns[0].reply)
	}
	if !strings.Contains(turns[1].prompt, "[code-tree · src · lru-cache") {
		t.Errorf("the intercom message is missing from the prompt: %q", turns[1].prompt)
	}
}

// A message sent with t on a box lands on that box.
func TestPointedAtRoutesToTheBox(t *testing.T) {
	tree := convoTree(t)
	id := apply(tree, "c", placement{thread: "new", parent: "new-topic", topic: "S", title: "LRU cache", status: "open"})
	g := load(tree)
	if got := pointedAt("[intercom] ...\n[code-tree · src · "+id+" · LRU cache]\nwhy?", g); got != id {
		t.Fatalf("pointedAt = %q, want %q", got, id)
	}
	if got := pointedAt("no tag here", g); got != "" {
		t.Fatalf("pointedAt without a tag = %q", got)
	}
}

// Helper Claudes must not look like the pane's agent or build trees of their own.
func TestHelperEnvIsClean(t *testing.T) {
	os.Setenv("HERDR_PANE_ID", "w1:p1")
	os.Setenv("IC_ID", "abc")
	defer os.Unsetenv("HERDR_PANE_ID")
	defer os.Unsetenv("IC_ID")
	env := strings.Join(helperEnv("CT_AGENT=x"), "\n")
	for _, bad := range []string{"HERDR_PANE_ID=", "IC_ID=abc"} {
		if strings.Contains(env, bad) {
			t.Errorf("helper env keeps %s", bad)
		}
	}
	for _, want := range []string{"CT_SKIP=1", "IC_SKIP=1", "CT_AGENT=x"} {
		if !strings.Contains(env, want) {
			t.Errorf("helper env lacks %s", want)
		}
	}
}
