package main

import (
	"strings"
	"testing"
)

// promptPane is a ct see pane with its talk prompt open; it records what gets sent.
func promptPane() (*seer, *[]string) {
	sent := &[]string{}
	s := &seer{results: make(chan string, 4), st: convoState{}}
	s.ask("talk", func(t string) { *sent = append(*sent, t) })
	return s, sent
}

// drive feeds reads to a pane like the terminal would, settling waits at the end.
func drive(s *seer, in *paneInput, reads ...string) bool {
	for _, r := range reads {
		if !in.read([]byte(r), s.key, s.paste) {
			return false
		}
	}
	if held, _ := in.p.pending(); held {
		return in.settle(s.key, s.paste)
	}
	return true
}

func chunks(s string, n int) []string {
	var out []string
	for len(s) > n {
		out, s = append(out, s[:n]), s[n:]
	}
	return append(out, s)
}

// A paste stays text however the terminal splits it into reads.
func TestPasteSplitAnywhereStaysText(t *testing.T) {
	paste := pasteStart + strings.Repeat("x", 40) + "\nquit the old flag\n\ndo it Aé 🎉\n" + pasteEnd
	want := strings.Repeat("x", 40) + " quit the old flag do it Aé 🎉"
	for n := 1; n <= len(paste); n++ {
		s, sent := promptPane()
		in := newPaneInput()
		if !drive(s, in, append(chunks(paste, n), "\r")...) {
			t.Fatalf("split every %d bytes: the pane closed", n)
		}
		if len(*sent) != 1 || (*sent)[0] != want {
			t.Fatalf("split every %d bytes: sent %q", n, *sent)
		}
	}
}

// A real Enter always sends, even when more keys share its read.
func TestEnterSendsEvenWithKeysInTheSameRead(t *testing.T) {
	s, sent := promptPane()
	in := newPaneInput()
	if drive(s, in, "ok\rq") { // q after Enter is a real key: it closes the pane
		t.Fatal("q after Enter did not act as a key")
	}
	if len(*sent) != 1 || (*sent)[0] != "ok" {
		t.Fatalf("sent %q", *sent)
	}
}

// With no prompt open, a paste is ignored: never run as keys.
func TestPasteWithNoPromptIsNeverKeys(t *testing.T) {
	s := &seer{results: make(chan string, 4), st: convoState{}}
	in := newPaneInput()
	if !drive(s, in, pasteStart+"q\ndrop everything\n"+pasteEnd) {
		t.Fatal("a pasted q closed the pane")
	}
	if !strings.Contains(s.msg, "paste ignored") {
		t.Fatalf("msg = %q", s.msg)
	}
}

// Text written by agents can't carry terminal control sequences to the screen.
func TestAgentTextIsCleaned(t *testing.T) {
	g := &Graph{Nodes: map[string]*Node{"a": {Title: "hi\x1b]52;c;ZXZpbA==\x07 there", Outcome: "\x1b[2Jgone", Body: "log\n\x1b[31mred"}}}
	cleanGraph(g)
	n := g.Nodes["a"]
	for _, s := range []string{n.Title, n.Outcome, n.Body} {
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("control characters survived: %q", s)
		}
	}
}
