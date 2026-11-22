package main

// herdr: start a new agent on a node in its own tab. Optional; outside herdr
// `ct run` explains itself and everything else works the same.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func herdrBin() string {
	if os.Getenv("HERDR_ENV") == "" && os.Getenv("HERDR_SOCKET_PATH") == "" {
		return ""
	}
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	b, _ := exec.LookPath("herdr")
	return b
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func cmdRun(a *Args) {
	need(a, 1, "run")
	bin := herdrBin()
	if bin == "" {
		die("ct run opens a herdr tab, so it only works inside herdr")
	}
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if st := g.Info[i].State; st != "open" && !a.Bool("force") {
		die("%s is %s; only open nodes get a new agent (or --force)", i, g.Info[i].Label)
	}
	agent := a.Get("agent", "claude")
	args := []string{"tab", "create", "--cwd", loadConfig(g.Dir).Code, "--label", i, "--focus"}
	if ws := os.Getenv("HERDR_WORKSPACE_ID"); ws != "" {
		args = append(args, "--workspace", ws)
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		die("herdr tab create failed: %v", err)
	}
	var res struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &res) != nil || res.Result.RootPane.PaneID == "" {
		die("could not read the new herdr pane: %s", strings.TrimSpace(string(out)))
	}
	pane := res.Result.RootPane.PaneID
	prompt := fmt.Sprintf("You own code-tree node `%s` (%s). Run `ct show %s`, then `ct claim %s`, and work on it "+
		"following .tree/PROTOCOL.md. Ask the human with `ct ask` when you need a decision, and finish with "+
		"`ct ready %s --outcome \"...\"`.", i, n.Title, i, i, i)
	if err := exec.Command(bin, "pane", "run", pane, agent+" "+shellQuote(prompt)).Run(); err != nil {
		die("could not start %s in %s: %v", agent, pane, err)
	}
	appendLog(n, fmt.Sprintf("new %s agent started in herdr pane %s", agent, pane), who(a))
	g.mutate(n)
	fmt.Printf("%s: new %s agent in herdr pane %s\n", i, agent, pane)
}
