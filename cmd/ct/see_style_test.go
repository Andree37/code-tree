package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func demoSeer(t *testing.T) (*seer, map[string]string) {
	t.Helper()
	dir := newTree(t)
	ids := map[string]string{}
	ids["root"] = mustCT(t, dir, nil, "add", "Run the game in a page")
	ids["touch"] = mustCT(t, dir, nil, "add", "Touch controls", "-b", ids["root"])
	ids["data"] = mustCT(t, dir, nil, "add", "Touch data", "-b", ids["touch"])
	mustCT(t, dir, nil, "done", ids["data"], "-o", "ok")
	ids["ux"] = mustCT(t, dir, nil, "add", "Drag feel", "-b", ids["touch"])
	mustCT(t, dir, nil, "ask", ids["ux"], "Snap back or fade?")
	ids["tips"] = mustCT(t, dir, nil, "add", "Tooltips", "-b", ids["touch"])
	mustCT(t, dir, nil, "decide", ids["touch"], "no gestures")
	s := &seer{tree: filepath.Join(dir, treeDir), st: convoState{"current": ids["tips"]}, results: make(chan string, 4), w: 160, h: 40}
	s.g = load(s.tree)
	s.head, s.sel = ids["root"], ids["root"]
	return s, ids
}

// Badges count what is below a card: waiting for you, still open, decisions, settled points.
func TestCardBadges(t *testing.T) {
	s, ids := demoSeer(t)
	b := s.badges(ids["touch"])
	if b.you != 1 || b.open != 1 || b.decisions != 1 || b.settled != 1 || b.kids != 3 {
		t.Fatalf("touch badges: %+v", b)
	}
	if b := s.badges(ids["root"]); b.you != 1 || b.open != 2 || b.kids != 1 {
		t.Fatalf("root badges: %+v", b)
	}
}

// The pane draws the cards, the badges and the legend, and no stray escapes.
func TestCardsRender(t *testing.T) {
	s, _ := demoSeer(t)
	out := stripSGR(captureStdout(t, func() { s.render() }))
	for _, want := range []string{"Touch controls", " !1 ", " ?1 ", " D1 ", "1/3", "●", "▼", "waiting for you", "for you"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pane is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(termSafe(out), "\x1b]") {
		t.Fatal("OSC escape in the pane")
	}
}

// + adds the new idea under the card at once and selects it.
func TestBranchAddsChild(t *testing.T) {
	s, ids := demoSeer(t)
	tree := s.tree // the pane's reload re-resolves its tree, which a test has no session for
	s.branch(ids["data"], "try haptics on drag")
	waitChildren(5 * time.Second)
	g := load(tree)
	n := g.Nodes[s.sel]
	if n == nil || len(n.Blocks) != 1 || n.Blocks[0] != ids["data"] {
		t.Fatalf("new idea not under the card: sel=%q node=%+v", s.sel, n)
	}
	if !strings.Contains(n.Title, "haptics") {
		t.Fatalf("title: %q", n.Title)
	}
	if st := g.Nodes[ids["data"]].Status; st != "open" {
		t.Fatalf("a settled card with a new idea under it should reopen, got %q", st)
	}
}

// Colours fall back to the 256-colour palette sensibly.
func TestCube256(t *testing.T) {
	for _, c := range []struct {
		in   rgb
		want int
	}{{rgb{0, 0, 0}, 16}, {rgb{255, 255, 255}, 231}, {rgb{255, 0, 0}, 196}} {
		if got := cube256(c.in); got != c.want && !(c.want == 16 && got == 232) && !(c.want == 231 && got == 255) {
			t.Errorf("cube256(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ct where gives herdr's sidebar each session's card and counts, one tab-separated line each.
func TestWhereForSidebar(t *testing.T) {
	s, ids := demoSeer(t)
	home := t.TempDir()
	id := "abc-123"
	dir := filepath.Join(home, "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := "tree " + s.tree + "\ncurrent " + ids["tips"] + "\npackage src/ui\nrepo /x\n"
	if err := os.WriteFile(filepath.Join(dir, "convo"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"CT_HOME=" + home}
	out := mustCT(t, home, env, "where", id, "unknown-session", "../escape")
	if out != id+"\tsrc/ui\tTooltips\t1\t2" {
		t.Fatalf("ct where: %q", out)
	}
}
