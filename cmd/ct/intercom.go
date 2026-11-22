package main

// ct speaks through intercom (github.com/Andree37/intercom, the `ic` binary):
// who is acting, talking to the agent on a node, and telling a node's owner
// when the human changes it. Without ic, ct still works; only talking is off.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const icInstall = "go install github.com/Andree37/intercom/cmd/ic@latest && ic install claude"

func icBin() string {
	p, _ := exec.LookPath("ic")
	return p
}

// icMe is this agent session's intercom name, or "" outside one.
func icMe() string {
	if os.Getenv("IC_ID") == "" && os.Getenv("CLAUDECODE") != "1" {
		return ""
	}
	bin := icBin()
	if bin == "" {
		return ""
	}
	out, err := exec.Command(bin, "me").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// icSend delivers a message about a node. wake=false leaves an idle agent idle.
func icSend(to string, nodeID, text string, wake bool) (string, error) {
	bin := icBin()
	if bin == "" {
		return "", fmt.Errorf("talking needs intercom: %s", icInstall)
	}
	args := []string{"send", "--topic", "ct:" + nodeID}
	if !wake {
		args = append(args, "--no-wake")
	}
	args = append(args, "--", to, text)
	out, err := exec.Command(bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var logLine = regexp.MustCompile(`^- \S+ \S+ \[([^\]]+)\] (.*)$`)

// logWho returns who wrote the last log line whose text starts with prefix.
func logWho(n *Node, contains string) string {
	who := ""
	for _, l := range strings.Split(n.Body, "\n") {
		if m := logLine.FindStringSubmatch(l); m != nil && strings.Contains(m[2], contains) {
			who = m[1]
		}
	}
	return who
}

func isAgent(w string) bool { return w != "" && w != "human" && w != "ct sync" }

// tell lets the agents involved with a node know what the human just did to it:
// its owner, whoever asked its open question, and whoever proposed it.
func tell(g *Graph, n *Node, actor, what string, wake bool) {
	if actor != "human" || icBin() == "" {
		return
	}
	seen := map[string]bool{}
	for _, t := range []string{n.Agent, logWho(n, "QUESTION:"), logWho(n, "created")} {
		if !isAgent(t) || seen[t] {
			continue
		}
		seen[t] = true
		body := fmt.Sprintf("[ct · %s · %s] The human %s.\n(details: ct show %s)", n.ID, n.Title, what, n.ID)
		_, _ = icSend(t, n.ID, body, wake)
	}
}

func cmdTalk(a *Args) {
	need(a, 2, "talk")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	me := who(a)
	to := a.Get("to", "")
	if to == "" {
		if isAgent(n.Agent) && n.Agent != me {
			to = n.Agent
		} else {
			to = "tab"
		}
	}
	text := strings.Join(a.Pos[1:], " ")
	body := fmt.Sprintf("[ct · %s · %s · %s]\n%s\n(context: ct show %s)", i, n.Title, g.Info[i].Label, text, i)
	out, err := icSend(to, i, body, true)
	if err != nil {
		die("%s", strings.TrimPrefix(out, "ic: "))
	}
	appendLog(n, fmt.Sprintf("talk → %s: %s", to, oneLine(text)), me)
	g.mutate(n)
	fmt.Println(out)
}

func cmdWork(a *Args) {
	need(a, 1, "work")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	a.Pos = []string{i, fmt.Sprintf("Please work on this node now: run `ct show %s`, then `ct claim %s`, and follow .tree/PROTOCOL.md. "+
		"When finished, `ct ready %s --outcome \"...\"`.", i, i, i)}
	cmdTalk(a)
}

// overlaps reports paths two nodes' touches have in common (either side may be a pattern).
func overlaps(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y || match(x, y) || match(y, x) || strings.HasPrefix(x, y) || strings.HasPrefix(y, x) {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// warnOverlap tells the claimer, and the other owners, about shared files.
func warnOverlap(g *Graph, i, me string) {
	n := g.Nodes[i]
	for _, j := range g.IDs {
		o := g.Nodes[j]
		if j == i || g.Info[j].State != "working" || o.Agent == me {
			continue
		}
		if paths := overlaps(n.Touches, o.Touches); len(paths) > 0 {
			fmt.Fprintf(os.Stderr, "warning: %s (%s) is working on %s, which also touches %s. Coordinate: ct talk %s \"...\"\n",
				dash(o.Agent), j, o.Title, strings.Join(paths, ", "), j)
			if isAgent(o.Agent) {
				_, _ = icSend(o.Agent, j, fmt.Sprintf("[ct · %s] %s just claimed `%s` (%s), which also touches %s. Agree who edits what before you both change them.",
					j, me, i, n.Title, strings.Join(paths, ", ")), false)
			}
		}
	}
}
