package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets tests run the real CLI: with CT_TEST_MAIN=1 this test binary
// behaves exactly like `ct`.
func TestMain(m *testing.M) {
	if os.Getenv("CT_TEST_MAIN") == "1" {
		own := os.Args[1:] // started by runSelf (os.Executable): its own argv is the command
		os.Args = append([]string{"ct"}, strings.Fields(os.Getenv("CT_TEST_ARGS"))...)
		if raw := os.Getenv("CT_TEST_ARGV"); raw != "" { // exact argv, tab separated
			os.Args = append([]string{"ct"}, strings.Split(raw, "\t")...)
		} else if os.Getenv("CT_TEST_ARGS") == "" && len(own) > 0 && !strings.HasPrefix(own[0], "-test.") {
			os.Args = append([]string{"ct"}, own...)
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ct runs the CLI in dir with extra environment, returning output and success.
func ct(t *testing.T, dir string, env []string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), "CT_TEST_MAIN=1", "CT_TEST_ARGV="+strings.Join(args, "\t"), "CT_HOME="+filepath.Join(dir, ".cthome"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// cleanEnv drops anything that would make ct think an agent or herdr is involved.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "CT_AGENT", k == "CLAUDECODE", k == "IC_ID", k == "CT_TREE", k == "CT_HOME",
			strings.HasPrefix(k, "HERDR_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// newTree makes an empty project with `ct init` and returns its folder.
func newTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, ok := ct(t, dir, nil, "init"); !ok {
		t.Fatalf("init: %s", out)
	}
	return dir
}

func mustCT(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, ok := ct(t, dir, env, args...)
	if !ok {
		t.Fatalf("ct %v failed: %s", args, out)
	}
	return strings.TrimSpace(out)
}

var agentEnv = []string{"CT_AGENT=bot"}
