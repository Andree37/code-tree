package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Node text must never be able to end the page's data block or become markup.
func TestPageDataCannotBreakOut(t *testing.T) {
	dir := newTree(t)
	evil := `x </SCRIPT><img src=x onerror=alert(1)> <!--<script> </div>`
	mustCT(t, dir, nil, "add", evil)
	page := renderHTML(filepath.Join(dir, treeDir), "")
	start := strings.Index(page, `<div id="data" hidden>`)
	end := strings.Index(page[start:], "</div>")
	if start < 0 || end < 0 {
		t.Fatal("data element not found")
	}
	data := page[start : start+end]
	for _, bad := range []string{"<img", "</SCRIPT", "<!--", "<script"} {
		if strings.Contains(data, bad) {
			t.Errorf("raw %q leaked into the page data: %s", bad, data)
		}
	}
	if !strings.Contains(data, "&lt;img") {
		t.Errorf("title was not kept (escaped): %s", data)
	}
}

// A page on another domain rebound to 127.0.0.1 must be refused.
func TestOnlyLocalHostsServed(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7337": true, "localhost:7337": true, "[::1]:7337": true,
		"evil.example:7337": false, "127.0.0.1:9999": false, "": false,
	} {
		if got := hostAllowed(host, "7337"); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// An agent cannot pass itself off as the human.
func TestAgentCannotClaimHuman(t *testing.T) {
	os.Setenv("CT_AGENT", "bot")
	defer os.Unsetenv("CT_AGENT")
	a := parseArgs("note", []string{"--by", "human"}, []flagSpec{fBy})
	if got := who(a); got == "human" {
		t.Fatal("agent with --by human was treated as the human")
	}
}

// Checks run shell commands, so only the human may set or change them, and
// only the human may start agents with a command of their choice.
func TestAgentsCannotSetChecksOrRunCommands(t *testing.T) {
	dir := newTree(t)
	id := mustCT(t, dir, nil, "add", "Goal")
	marker := filepath.Join(dir, "ran")
	for _, args := range [][]string{
		{"check", id, "--set", "touch " + marker},
		{"check", id, "--set", "touch " + marker, "--by", "human"},
		{"check", id, "--clear"},
		{"add", "Sub", "-b", id, "--check", "touch " + marker},
		{"run", id, "--agent", "touch " + marker},
	} {
		if out, ok := ct(t, dir, agentEnv, args...); ok || !strings.Contains(out, "only the human") {
			t.Errorf("agent ran %v: ok=%v out=%s", args, ok, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an agent-supplied command was executed")
	}
	// the human still can
	if out, ok := ct(t, dir, nil, "check", id, "--set", "true"); !ok {
		t.Fatalf("human could not set a check: %s", out)
	}
}
