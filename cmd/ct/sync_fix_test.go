package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// setField rewrites one frontmatter line of a node file, as a hand edit or a PR would.
func setField(t *testing.T, dir, id, key, value string) {
	t.Helper()
	p := filepath.Join(dir, treeDir, "nodes", id+".md")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^` + key + `: .*$`)
	line := key + ": " + value
	var out string
	if re.Match(data) {
		out = re.ReplaceAllLiteralString(string(data), line)
	} else {
		out = strings.Replace(string(data), "---\n", "---\n"+line+"\n", 1)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, repo, name, body string) {
	t.Helper()
	write(t, filepath.Join(repo, name), body)
	git_(t, repo, "add", "-A")
	git_(t, repo, "commit", "-qm", "change "+name)
}

// A node's `at:` that looks like a git option is never passed to git as one.
func TestSyncAtIsNeverAGitOption(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a"})
	victim := filepath.Join(t.TempDir(), "victim.txt")
	write(t, victim, "precious")
	mustCT(t, repo, nil, "init")
	id := mustCT(t, repo, nil, "add", "A", "-t", "a.go")
	mustCT(t, repo, nil, "done", id)
	mustCT(t, repo, nil, "sync")
	setField(t, repo, id, "at", "--output="+victim)
	commitFile(t, repo, "a.go", "a2")
	out := mustCT(t, repo, nil, "sync")
	if data, _ := os.ReadFile(victim); string(data) != "precious" {
		t.Fatalf("sync wrote to a file named in a node's at: %q", data)
	}
	if !strings.Contains(out, "stale    "+id) {
		t.Fatalf("a node whose at is not a commit should count the change:\n%s", out)
	}
}

// A base that looks like a git option never reaches git either.
func TestChangedSinceRejectsOptionLikeBase(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a"})
	victim := filepath.Join(t.TempDir(), "victim.txt")
	write(t, victim, "precious")
	cfg := Config{Code: repo}
	_ = changedSince(cfg, "--output="+victim)
	if _, err := diffNames(cfg, "--output="+victim, "HEAD"); err == nil {
		t.Fatal("diffNames accepted an option as a revision")
	}
	if data, _ := os.ReadFile(victim); string(data) != "precious" {
		t.Fatalf("victim overwritten: %q", data)
	}
}

// Dropped work is never stale, so it cannot fail `sync --check`.
func TestSyncDroppedNodesNeverGoStale(t *testing.T) {
	repo := repoWith(t, map[string]string{"b.go": "b"})
	mustCT(t, repo, nil, "init")
	id := mustCT(t, repo, nil, "add", "Old idea", "-t", "b.go")
	mustCT(t, repo, nil, "drop", id)
	mustCT(t, repo, nil, "sync")
	commitFile(t, repo, "b.go", "b2")
	if out, ok := ct(t, repo, nil, "sync", "--check"); !ok || strings.Contains(out, "stale    "+id) {
		t.Fatalf("dropped node went stale: ok=%v\n%s", ok, out)
	}
}

// A node resolved at a commit that no longer exists still sees later changes.
func TestSyncAtCommitGoneStillCountsChanges(t *testing.T) {
	repo := repoWith(t, map[string]string{"b.go": "b"})
	mustCT(t, repo, nil, "init")
	id := mustCT(t, repo, nil, "add", "B work", "-t", "b.go")
	mustCT(t, repo, nil, "done", id)
	mustCT(t, repo, nil, "sync")
	setField(t, repo, id, "at", strings.Repeat("ab", 20)) // a commit that was rebased away
	commitFile(t, repo, "b.go", "b2")
	if out, ok := ct(t, repo, nil, "sync", "--check"); ok || !strings.Contains(out, "stale    "+id) {
		t.Fatalf("change after a vanished resolution commit was missed: ok=%v\n%s", ok, out)
	}
}

// A touch on a root file does not claim same-named files in subfolders.
func TestSyncRootTouchIsExact(t *testing.T) {
	repo := repoWith(t, map[string]string{"main.go": "m", "cmd/tool/main.go": "t"})
	mustCT(t, repo, nil, "init")
	id := mustCT(t, repo, nil, "add", "Root main", "-t", "main.go")
	mustCT(t, repo, nil, "done", id)
	mustCT(t, repo, nil, "sync")
	commitFile(t, repo, "cmd/tool/main.go", "t2")
	out := mustCT(t, repo, nil, "sync")
	if strings.Contains(out, "stale    "+id) || !strings.Contains(out, "unmapped cmd/tool/main.go") {
		t.Fatalf("root main.go touch matched cmd/tool/main.go:\n%s", out)
	}
}

// Files with non-ASCII names are matched by their real path.
func TestSyncNonASCIIPaths(t *testing.T) {
	repo := repoWith(t, map[string]string{"docs/café.md": "c", "x.go": "x"})
	mustCT(t, repo, nil, "init")
	id := mustCT(t, repo, nil, "add", "Docs", "-t", "docs/")
	mustCT(t, repo, nil, "done", id)
	mustCT(t, repo, nil, "sync")
	commitFile(t, repo, "docs/café.md", "c2")
	commitFile(t, repo, "naïve.go", "n")
	out := mustCT(t, repo, nil, "sync")
	if !strings.Contains(out, "stale    "+id+"  (docs/café.md)") {
		t.Fatalf("docs/café.md change not seen:\n%s", out)
	}
	if !strings.Contains(out, "unmapped naïve.go") || strings.Contains(out, `\303`) {
		t.Fatalf("non-ASCII path reported quoted:\n%s", out)
	}
	if got := changedSince(Config{Code: repo}, ""); strings.Join(got, ",") != "" {
		// .tree and .cthome are untracked here; only check that names come out unquoted
		for _, p := range got {
			if strings.Contains(p, `"`) || strings.Contains(p, `\`) {
				t.Fatalf("status path quoted: %q", p)
			}
		}
	}
}

// Adding an ignore pattern clears paths already listed as unmapped.
func TestSyncNewIgnoreClearsUnmapped(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a"})
	mustCT(t, repo, nil, "init")
	mustCT(t, repo, nil, "sync")
	commitFile(t, repo, "web/yarn.lockfile", "l")
	if out := mustCT(t, repo, nil, "sync"); !strings.Contains(out, "unmapped web/yarn.lockfile") {
		t.Fatalf("setup: want it unmapped:\n%s", out)
	}
	cfgPath := filepath.Join(repo, treeDir, "config")
	data, _ := os.ReadFile(cfgPath)
	write(t, cfgPath, string(data)+"ignore = *.lockfile\n")
	if out, ok := ct(t, repo, nil, "sync", "--check"); !ok || strings.Contains(out, "yarn.lockfile") {
		t.Fatalf("ignored path still unmapped: ok=%v\n%s", ok, out)
	}
}

// An uncommitted edit to one of a node's files only excuses the commit of that file.
func TestSyncUncommittedOnlyCoversOwnFiles(t *testing.T) {
	repo := repoWith(t, map[string]string{"a.go": "a", "b.go": "b"})
	mustCT(t, repo, nil, "init")
	mustCT(t, repo, nil, "sync")
	id := mustCT(t, repo, nil, "add", "AB", "-t", "a.go,b.go")
	write(t, filepath.Join(repo, "a.go"), "a2") // its own work, not committed yet
	mustCT(t, repo, nil, "done", id)
	data, _ := os.ReadFile(filepath.Join(repo, treeDir, "nodes", id+".md"))
	if !strings.Contains(string(data), "uncommitted: a.go") {
		t.Fatalf("want the dirty path recorded:\n%s", data)
	}
	// someone else's change to b.go lands first, on its own
	git_(t, repo, "stash", "push", "-q", "--", "a.go")
	commitFile(t, repo, "b.go", "b2")
	git_(t, repo, "stash", "pop", "-q")
	out := mustCT(t, repo, nil, "sync")
	if !strings.Contains(out, "stale    "+id+"  (b.go)") {
		t.Fatalf("an unrelated change hid behind the uncommitted flag:\n%s", out)
	}
}

func TestConfigCommentsOnlyAfterSpace(t *testing.T) {
	for in, want := range map[string]string{
		"chat = run#1 --x # note": "chat = run#1 --x ",
		"# whole line":            "",
		"  # indented":            "  ",
		"code = ../a#b":           "code = ../a#b",
		"ignore = *.lock\t# tab":  "ignore = *.lock\t",
	} {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q, want %q", in, got, want)
		}
	}
}
