package main

// Steering: agents propose, humans decide.

import (
	"fmt"
	"strings"
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
	g.mutate(n)
	fmt.Printf("%s: Open\n", i)
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
}

func cmdMerge(a *Args) {
	need(a, 2, "merge")
	g := load(requireTree())
	i, into := g.resolve(a.Pos[0]), g.resolve(a.Pos[1])
	if i == into {
		die("cannot merge a node into itself")
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
	for _, c := range g.Info[i].Children { // children now block the target
		cn := g.Nodes[c]
		for k, b := range cn.Blocks {
			if b == i {
				cn.Blocks[k] = into
			}
		}
		changed = append(changed, cn)
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
