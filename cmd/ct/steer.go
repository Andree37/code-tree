package main

// Steering: agents propose, humans decide.

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

func cmdAccept(a *Args) {
	need(a, 1, "accept")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if n.Status != "proposed" {
		die("%s is %s, not proposed", i, label(n.Status))
	}
	n.Status = "open"
	msg := "accepted"
	if note := a.Get("note", ""); note != "" {
		msg += ": " + note
	}
	appendLog(n, msg, who(a))
	changed := []*Node{n}
	for _, p := range g.Info[i].Parents { // now it is real work: what it blocks is open again
		if pn := g.Nodes[p]; pn.Status == "done" || pn.Status == "ready" {
			pn.Status = "open"
			appendLog(pn, fmt.Sprintf("reopened: accepted prerequisite `%s`", i), who(a))
			changed = append(changed, pn)
		}
	}
	g.mutate(changed...)
	fmt.Printf("%s: Open\n", i)
	tell(g, n, who(a), "accepted it; it can be claimed now", false)
}

func cmdReject(a *Args) {
	need(a, 1, "reject")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if n.Status != "proposed" {
		die("%s is %s, not proposed (use `ct drop`)", i, label(n.Status))
	}
	n.Status = "dropped"
	msg := "rejected"
	if len(a.Pos) > 1 {
		msg += ": " + strings.Join(a.Pos[1:], " ")
	}
	appendLog(n, msg, who(a))
	g.mutate(n)
	fmt.Printf("%s: Dropped\n", i)
	tell(g, n, who(a), strings.Replace(msg, "rejected", "rejected it", 1), false)
}

func cmdMerge(a *Args) {
	need(a, 2, "merge")
	g := load(requireTree())
	i, into := g.resolve(a.Pos[0]), g.resolve(a.Pos[1])
	if i == into {
		die("cannot merge a node into itself")
	}
	if g.descendants(i)[into] {
		die("%s is below %s; merging it there would make a loop (merge the other way round)", into, i)
	}
	n, t := g.Nodes[i], g.Nodes[into]
	ctx := strings.TrimSpace(section(n.Body, "## Context"))
	note := fmt.Sprintf("merged `%s` (%s) into this", i, n.Title)
	if ctx != "" {
		note += ": " + oneLine(ctx)
	}
	appendLog(t, note, who(a))
	t.Touches = append(t.Touches, n.Touches...)
	addTouches(t, nil)
	changed := []*Node{t, n}
	for _, c := range g.Info[i].Children { // children now block the target, once
		cn := g.Nodes[c]
		var blocks []string
		for _, b := range cn.Blocks {
			if b == i {
				b = into
			}
			if !slices.Contains(blocks, b) {
				blocks = append(blocks, b)
			}
		}
		cn.Blocks = blocks
		changed = append(changed, cn)
	}
	// what waited on the merged node now waits on the target, so nothing above
	// it can be finished while the merged work is still open
	under := g.descendants(into)
	for _, p := range g.Info[i].Parents {
		if p != into && !under[p] && !slices.Contains(t.Blocks, p) {
			t.Blocks = append(t.Blocks, p)
		}
	}
	n.Status = "dropped"
	appendLog(n, fmt.Sprintf("merged into `%s`", into), who(a))
	g.mutate(changed...)
	fmt.Printf("%s merged into %s\n", i, into)
}

// section returns the text under a "## Heading" up to the next heading.
func section(body, heading string) string {
	_, rest, ok := strings.Cut(body, heading+"\n")
	if !ok {
		return ""
	}
	if k := strings.Index(rest, "\n## "); k >= 0 {
		rest = rest[:k]
	}
	return rest
}

// runCheck runs a node's check in the code repo and returns (passed, output).
func runCheck(g *Graph, n *Node) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", n.Check)
	cmd.Dir = loadConfig(g.Dir).Code
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func lastLines(s string, k int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > k {
		lines = lines[len(lines)-k:]
	}
	return strings.Join(lines, "\n")
}

func cmdCheck(a *Args) {
	need(a, 1, "check")
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	if a.Bool("clear") || a.Has("set") {
		humanOnly(a, "change a check (it runs as a shell command)")
	}
	switch {
	case a.Bool("clear"):
		n.Check = ""
		appendLog(n, "check removed", who(a))
		g.mutate(n)
		return
	case a.Has("set"):
		n.Check = oneLine(a.Get("set", ""))
		appendLog(n, "check set: "+n.Check, who(a))
		g.mutate(n)
	}
	if n.Check == "" {
		die("%s has no check (set one with --set \"cmd\")", i)
	}
	ok, out := runCheck(g, n)
	if out != "" {
		fmt.Println(lastLines(out, 15))
	}
	if !ok {
		die("check failed: %s", n.Check)
	}
	fmt.Println("check passed:", n.Check)
}

// runStaleChecks re-runs checks of stale nodes: pass clears stale, fail reopens.
func runStaleChecks(g *Graph, head string) []*Node {
	var changed []*Node
	for _, i := range g.IDs {
		n := g.Nodes[i]
		if len(n.Stale) == 0 || n.Check == "" || n.Status == "dropped" { // dropped work is never reopened
			continue
		}
		ok, out := runCheck(g, n)
		// the check may have taken minutes: apply the result to what is on disk now
		fresh := load(g.Dir)
		if n = fresh.Nodes[i]; n == nil || len(n.Stale) == 0 || n.Status == "dropped" {
			continue
		}
		g.Nodes[i] = n
		if ok {
			appendLog(n, "check passed after changes to "+strings.Join(n.Stale, ", ")+"; no longer stale", "ct sync")
			n.Stale, n.At = nil, head
			fmt.Printf("  check ok %s\n", i)
		} else {
			appendLog(n, "check failed after changes to "+strings.Join(n.Stale, ", ")+"; reopened: "+oneLine(lastLines(out, 3)), "ct sync")
			n.Status, n.Stale = "open", nil
			fmt.Printf("  check FAILED %s, reopened\n", i)
		}
		changed = append(changed, n)
	}
	return changed
}

func addDecision(a *Args, cmd, prefix string) {
	need(a, 2, cmd)
	g := load(requireTree())
	i := g.resolve(a.Pos[0])
	n := g.Nodes[i]
	d := oneLine(strings.Join(a.Pos[1:], " "))
	n.Decisions = append(n.Decisions, d)
	if cmd == "answer" && n.Status == "needs-you" {
		n.Status = "open"
	}
	appendLog(n, prefix+d, who(a))
	g.mutate(n)
	if cmd == "answer" {
		tell(g, n, who(a), "answered: "+d, true)
	} else {
		tell(g, n, who(a), "decided: "+d, false)
		for c := range g.descendants(i) { // decisions bind everything below
			if g.Info[c].State == "working" {
				tell(g, g.Nodes[c], who(a), "decided on `"+i+"` above you: "+d, false)
			}
		}
	}
	fmt.Printf("%s: %s · decision recorded for it and everything below\n", i, label(n.Status))
}

func cmdAnswer(a *Args) { addDecision(a, "answer", "ANSWER: ") }
func cmdDecide(a *Args) { addDecision(a, "decide", "DECISION: ") }
