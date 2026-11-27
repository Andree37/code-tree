package main

import (
	"fmt"
	"os"
	"strings"
)

// The look of the graph pane: filled cards in a few muted colours, small
// badges, and a glow around the card Claude is on. Colours are 24-bit when the
// terminal says it can, and the nearest of the 256 standard ones otherwise.

type rgb struct{ r, g, b int }

var trueColor = func() bool {
	c := strings.ToLower(os.Getenv("COLORTERM"))
	return c == "truecolor" || c == "24bit"
}()

// cube256 is the nearest colour of xterm's 6x6x6 cube or grey ramp.
func cube256(c rgb) int {
	level := func(v int) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (v - 35) / 40
	}
	r, g, b := level(c.r), level(c.g), level(c.b)
	steps := []int{0, 95, 135, 175, 215, 255}
	dist := func(x, y, z int) int {
		return (c.r-x)*(c.r-x) + (c.g-y)*(c.g-y) + (c.b-z)*(c.b-z)
	}
	cube := 16 + 36*r + 6*g + b
	best := dist(steps[r], steps[g], steps[b])
	avg := (c.r + c.g + c.b) / 3
	k := min(23, max(0, (avg-8)/10))
	gv := 8 + 10*k
	if dist(gv, gv, gv) < best {
		return 232 + k
	}
	return cube
}

func fg(c rgb) string {
	if trueColor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", cube256(c))
}

func bg(c rgb) string {
	if trueColor {
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[48;5;%dm", cube256(c))
}

// A card style: its fill, its text, and its "+" chip.
type cardStyle struct {
	fill, text, chip rgb
	bold             bool
}

var (
	styleSubject = cardStyle{fill: rgb{43, 47, 56}, text: rgb{200, 204, 212}, chip: rgb{62, 67, 78}}
	styleOpen    = cardStyle{fill: rgb{31, 59, 112}, text: rgb{255, 255, 255}, chip: rgb{48, 80, 143}, bold: true}
	styleSettled = cardStyle{fill: rgb{30, 58, 42}, text: rgb{223, 240, 227}, chip: rgb{46, 82, 61}, bold: true}
	styleWaiting = cardStyle{fill: rgb{74, 50, 20}, text: rgb{255, 217, 160}, chip: rgb{102, 72, 32}, bold: true}
	styleDropped = cardStyle{fill: rgb{34, 37, 43}, text: rgb{110, 115, 125}, chip: rgb{48, 52, 60}}

	colGlow     = rgb{229, 192, 74}  // the card Claude is on
	colSelected = rgb{255, 255, 255} // the card you picked
	colEdge     = rgb{140, 146, 156}
	colEdgeHot  = rgb{255, 150, 60}
	colMuted    = rgb{150, 156, 166}

	badgeYou   = [2]rgb{{92, 38, 38}, {255, 154, 154}} // bg, fg
	badgeOpen  = [2]rgb{{79, 74, 28}, {242, 220, 107}}
	badgeDecis = [2]rgb{{36, 58, 102}, {168, 193, 255}}
)

// styleFor picks a card's colours from its state.
func styleFor(f *Info) cardStyle {
	switch {
	case f.State == "needs-you":
		return styleWaiting
	case f.State == "dropped":
		return styleDropped
	case f.State == "done" || f.State == "ready":
		return styleSettled
	case f.Kind == "head", f.State == "waiting":
		return styleSubject // a subject, or a point whose own sub-points are still open
	}
	return styleOpen
}

// badges counts, below a card: points waiting for you, points still open,
// decisions made on it, and how many of its direct points are settled.
type badgeCounts struct {
	you, open, decisions, settled, kids int
}

func (s *seer) badges(id string) badgeCounts {
	var b badgeCounts
	g := s.g
	if n := g.Nodes[id]; n != nil {
		b.decisions = len(n.Decisions)
	}
	for d := range g.descendants(id) {
		f := g.Info[d]
		if f == nil {
			continue
		}
		switch {
		case f.State == "needs-you":
			b.you++
		case !resolved(f.State) && f.State != "ready":
			b.open++
		}
	}
	for _, c := range g.Info[id].Children {
		b.kids++
		if st := g.Info[c].State; st == "done" || st == "ready" || st == "dropped" {
			b.settled++
		}
	}
	return b
}

// chips renders the badge row as (text, colour) pieces.
func (b badgeCounts) chips(st cardStyle) [][2]string {
	var out [][2]string
	chip := func(text string, c [2]rgb) {
		out = append(out, [2]string{" " + text + " ", bg(c[0]) + fg(c[1])})
	}
	if b.you > 0 {
		chip(fmt.Sprintf("!%d", b.you), badgeYou)
	}
	if b.open > 0 {
		chip(fmt.Sprintf("?%d", b.open), badgeOpen)
	}
	if b.decisions > 0 {
		chip(fmt.Sprintf("D%d", b.decisions), badgeDecis)
	}
	if b.kids > 0 {
		out = append(out, [2]string{fmt.Sprintf("%d/%d", b.settled, b.kids), bg(st.fill) + fg(colMuted)})
	}
	return out
}

// legend explains the colours and badges in one line.
func legend() string {
	sw := func(st cardStyle, word string) string {
		return bg(st.fill) + "  " + cReset + " " + word + "  "
	}
	chip := func(c [2]rgb, text, word string) string {
		return bg(c[0]) + fg(c[1]) + " " + text + " " + cReset + " " + word + "  "
	}
	return " " + sw(styleOpen, "open") + sw(styleSettled, "settled") + sw(styleWaiting, "waiting for you") +
		sw(styleSubject, "subject") + fg(colGlow) + "●" + cReset + " Claude is here  " +
		chip(badgeYou, "!", "for you") + chip(badgeOpen, "?", "open") + chip(badgeDecis, "D", "decisions")
}

// stripSGR drops colour codes, to measure how wide a coloured line is.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
