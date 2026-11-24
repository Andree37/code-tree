package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFalseBoolFlagsAreFalse(t *testing.T) {
	a := parseArgs("done", []string{"--force=false", "--skip-check"}, []flagSpec{fForce, {"skip-check", "", false, ""}})
	if a.Bool("force") {
		t.Error("--force=false read as true")
	}
	if !a.Bool("skip-check") {
		t.Error("--skip-check read as false")
	}
}

func TestInitCodeKeepsConfig(t *testing.T) {
	dir := newTree(t)
	cfg := filepath.Join(dir, treeDir, "config")
	_ = os.WriteFile(cfg, []byte("code = .\napprove = none\n"), 0o644)
	mustCT(t, dir, nil, "init", "--code", "../app")
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "approve = none") || !strings.Contains(string(data), "code = ../app") {
		t.Fatalf("config not kept:\n%s", data)
	}
}

func TestAgentCanReclaimItsOwnNode(t *testing.T) {
	dir := newTree(t)
	id := mustCT(t, dir, nil, "add", "Work")
	mustCT(t, dir, agentEnv, "claim", id)
	if out, ok := ct(t, dir, agentEnv, "claim", id); !ok {
		t.Fatalf("agent could not re-claim its own node: %s", out)
	}
}

// A proposal is not real work yet: it must not reopen a ready parent, and a
// parent can't be finished while a proposal under it is undecided.
func TestProposalsAndParents(t *testing.T) {
	dir := newTree(t)
	parent := mustCT(t, dir, nil, "add", "Parent")
	mustCT(t, dir, nil, "ready", parent)
	stored := func(id string) string { return load(filepath.Join(dir, treeDir)).Nodes[id].Status }
	rejected := mustCT(t, dir, agentEnv, "add", "Bad idea", "-b", parent)
	if stored(parent) != "ready" {
		t.Fatalf("a proposal reopened the ready parent: %s", stored(parent))
	}
	mustCT(t, dir, nil, "reject", rejected)
	if g := load(filepath.Join(dir, treeDir)); g.Info[parent].State != "ready" {
		t.Fatalf("after rejecting the proposal the parent should be Ready again, is %s", g.Info[parent].State)
	}
	prop := mustCT(t, dir, agentEnv, "add", "Idea", "-b", parent)
	mustCT(t, dir, nil, "accept", prop)
	if stored(parent) != "open" {
		t.Fatalf("accepting the proposal did not reopen the parent: %s", stored(parent))
	}
	// a proposed node with open points under it can't be marked done
	sub := mustCT(t, dir, agentEnv, "add", "Sub idea")
	mustCT(t, dir, nil, "add", "Child", "-b", sub)
	if out, ok := ct(t, dir, nil, "done", sub); ok {
		t.Fatalf("done with an open child under it: %s", out)
	}
}

func TestChecksAndGlobsKeepTheirCharacters(t *testing.T) {
	dir := newTree(t)
	id := mustCT(t, dir, nil, "add", "Work", "-t", "[ab]/x.go", "--check", `"./run" && echo "ok"`)
	n := load(filepath.Join(dir, treeDir)).Nodes[id]
	if n.Check != `"./run" && echo "ok"` {
		t.Errorf("check mangled: %q", n.Check)
	}
	if len(n.Touches) != 1 || n.Touches[0] != "[ab]/x.go" {
		t.Errorf("glob touch mangled: %q", n.Touches)
	}
	// older files still read
	if got := splitList(`["a", "b"]`); strings.Join(got, ",") != "a,b" {
		t.Errorf("legacy list = %q", got)
	}
}

func TestBareIgnoreNamesMatchAnyDepth(t *testing.T) {
	cfg := Config{Ignore: []string{"package-lock.json", "go.sum"}}
	if !ignored(cfg, "web/package-lock.json") || !ignored(cfg, "tools/go.sum") {
		t.Error("bare lockfile names should match in subfolders")
	}
	if ignored(cfg, "go.summary") {
		t.Error("bare name matched a different file")
	}
}

// --- code sync against a real git repo -------------------------------------

func git_(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git_(t, dir, "init", "-q")
	for p, body := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644)
	}
	git_(t, dir, "add", "-A")
	git_(t, dir, "commit", "-qm", "init")
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A code folder inside a bigger git repo: paths are relative to the folder.
func TestSyncInSubfolderOfRepo(t *testing.T) {
	repo := repoWith(t, map[string]string{"app/src/a.go": "a", "other/x.go": "x"})
	app := filepath.Join(repo, "app")
	mustCT(t, app, nil, "init")
	id := mustCT(t, app, nil, "add", "A", "-t", "src/a.go")
	mustCT(t, app, nil, "done", id)
	mustCT(t, app, nil, "sync")
	write(t, filepath.Join(app, "src/a.go"), "a2")
	write(t, filepath.Join(repo, "other/x.go"), "x2")
	git_(t, repo, "commit", "-qam", "change")
	out := mustCT(t, app, nil, "sync")
	if !strings.Contains(out, "stale") || strings.Contains(out, "other/x.go") {
		t.Fatalf("want a.go stale and nothing outside app/ reported:\n%s", out)
	}
}

// A deleted unowned file stops being reported; done-before-commit is not stale.
func TestSyncDeletedFilesAndEarlyDone(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a"})
	mustCT(t, repo, nil, "init")
	mustCT(t, repo, nil, "sync")
	write(t, filepath.Join(repo, "tmp.go"), "t")
	git_(t, repo, "add", "-A")
	git_(t, repo, "commit", "-qm", "add tmp")
	if out := mustCT(t, repo, nil, "sync"); !strings.Contains(out, "unmapped tmp.go") {
		t.Fatalf("setup: tmp.go should be unmapped:\n%s", out)
	}
	git_(t, repo, "rm", "-q", "tmp.go")
	git_(t, repo, "commit", "-qm", "rm tmp")
	if out, ok := ct(t, repo, nil, "sync", "--check"); !ok || strings.Contains(out, "tmp.go") {
		t.Fatalf("deleted file still unmapped: ok=%v\n%s", ok, out)
	}

	// done before committing its own work, then the commit
	id := mustCT(t, repo, nil, "add", "Edit a", "-t", "a.go")
	write(t, filepath.Join(repo, "a.go"), "a2")
	mustCT(t, repo, nil, "done", id)
	git_(t, repo, "commit", "-qam", "the work")
	if out := mustCT(t, repo, nil, "sync"); strings.Contains(out, "stale    "+id) {
		t.Fatalf("its own commit made it stale:\n%s", out)
	}
}

// In a shallow clone the last synced commit is missing: --check must fail, not pass.
func TestSyncCheckFailsInShallowClone(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a"})
	mustCT(t, repo, nil, "init")
	mustCT(t, repo, nil, "sync")
	git_(t, repo, "add", "-A")
	git_(t, repo, "commit", "-qm", "tree")
	write(t, filepath.Join(repo, "b.go"), "b")
	git_(t, repo, "add", "-A")
	git_(t, repo, "commit", "-qm", "more")
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	if out, ok := ct(t, clone, nil, "sync", "--check"); ok {
		t.Fatalf("sync --check passed in a shallow clone:\n%s", out)
	}
}
