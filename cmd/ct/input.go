package main

// Terminal input for the panes (ct see, ct watch): raw bytes in, whole keys and whole pastes out.
// Ported from github.com/Andree37/intercom (cmd/ic/input.go), which found these cases.
//
// A read can end anywhere: inside an escape sequence, inside a multi-byte
// character, or in the middle of a paste. The parser keeps whatever is
// incomplete and finishes it with the next read, so where the bytes happen to
// be split never changes what a key or a paste means.
//
// Pastes are told apart from typing by bracketed paste (the pane turns it on
// with \x1b[?2004h): the terminal wraps pasted text in \x1b[200~ ... \x1b[201~.
// A line break inside a paste is text; a line break outside one is Enter.

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
	// A lone Esc is only an Esc once nothing follows it for this long;
	// otherwise it is the start of an arrow, function or Alt key.
	escWait = 30 * time.Millisecond
	// A paste whose end marker never comes is taken as finished after this.
	pasteWait = time.Second
)

// inputEvent is one key ("q", "\r", "\x1b[A", "\x1bb") or one whole paste.
type inputEvent struct {
	key     string
	paste   string
	isPaste bool
}

type inputParser struct {
	buf     []byte
	inPaste bool
	pasted  []byte
}

// pending says whether input is held back, waiting for more bytes, and how long
// to wait before taking it as it is (see flush).
func (p *inputParser) pending() (bool, time.Duration) {
	if p.inPaste {
		return true, pasteWait
	}
	return len(p.buf) > 0, escWait
}

// push adds one read and returns every event it completes.
func (p *inputParser) push(b []byte) []inputEvent {
	p.buf = append(p.buf, b...)
	return p.parse(false)
}

// flush is called when no more bytes came within the wait: a held Esc is a
// real Esc, a cut-off sequence is taken as is, an unfinished paste is done.
func (p *inputParser) flush() []inputEvent {
	return p.parse(true)
}

func (p *inputParser) parse(final bool) []inputEvent {
	var out []inputEvent
	for {
		if p.inPaste {
			if i := bytes.Index(p.buf, []byte(pasteEnd)); i >= 0 {
				p.pasted = append(p.pasted, p.buf[:i]...)
				p.buf = p.buf[i+len(pasteEnd):]
				out = append(out, inputEvent{paste: string(p.pasted), isPaste: true})
				p.inPaste, p.pasted = false, nil
				continue
			}
			if final {
				p.pasted = append(p.pasted, p.buf...)
				p.buf = nil
				out = append(out, inputEvent{paste: string(p.pasted), isPaste: true})
				p.inPaste, p.pasted = false, nil
				return out
			}
			// Keep a possible start of the end marker; the rest is pasted text.
			keep := 0
			for k := len(pasteEnd) - 1; k > 0; k-- {
				if bytes.HasSuffix(p.buf, []byte(pasteEnd[:k])) {
					keep = k
					break
				}
			}
			p.pasted = append(p.pasted, p.buf[:len(p.buf)-keep]...)
			p.buf = p.buf[len(p.buf)-keep:]
			return out
		}
		if len(p.buf) == 0 {
			return out
		}
		n, key, complete := nextKey(p.buf)
		if !complete {
			if !final {
				return out
			}
			if p.buf[0] == 0x1b { // held Esc: a real Esc; what followed is parsed on its own
				n, key = 1, "\x1b"
			} else { // a broken character: drop the byte
				p.buf = p.buf[1:]
				continue
			}
		}
		p.buf = p.buf[n:]
		if key == pasteStart {
			p.inPaste = true
			continue
		}
		if key != "" {
			out = append(out, inputEvent{key: key})
		}
	}
}

// nextKey reads one key from the front of b: its length, the key, and whether
// it is complete (false means more bytes are needed to know).
func nextKey(b []byte) (int, string, bool) {
	if b[0] != 0x1b {
		if !utf8.FullRune(b) {
			return 0, "", false
		}
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			return 1, "", true // invalid byte: skip it
		}
		return size, string(b[:size]), true
	}
	if len(b) == 1 {
		return 0, "", false
	}
	switch b[1] {
	case '[': // CSI: ESC [ params final
		for j := 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				return j + 1, string(b[:j+1]), true
			}
			if j > 32 { // not a real sequence: give up on it
				return j + 1, "", true
			}
		}
		return 0, "", false
	case 'O': // SS3: F1-F4 and application-mode arrows
		if len(b) < 3 {
			return 0, "", false
		}
		return 3, string(b[:3]), true
	case 0x1b: // Esc Esc: the first one stands alone
		return 1, "\x1b", true
	}
	if !utf8.FullRune(b[1:]) { // Alt+key: ESC then the key
		return 0, "", false
	}
	_, size := utf8.DecodeRune(b[1:])
	return 1 + size, string(b[:1+size]), true
}

// pasteText turns a paste into one line of draft text: line breaks (and blank
// lines) become a single space, tabs become spaces, control characters go.
func pasteText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(clean(strings.ReplaceAll(l, "\t", " ")), " "); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " ")
}

// clean removes what could steer a terminal from text that came from someone
// else (an agent's message, a session name): escape and other control
// characters. Tabs become spaces; line breaks stay, for callers that split.
func clean(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r == '\n' || !unicode.IsControl(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- ct panes

// paneInput connects the parser to a pane: whole keys go to key, whole pastes
// to paste, and a held Esc or unfinished paste is settled after its wait.
type paneInput struct {
	p    inputParser
	wait *time.Timer
}

func newPaneInput() *paneInput {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &paneInput{wait: t}
}

// handle runs events and returns false when the pane should close.
func (pi *paneInput) handle(events []inputEvent, key func(string) bool, paste func(string)) bool {
	for _, ev := range events {
		if ev.isPaste {
			paste(ev.paste)
		} else if !key(ev.key) {
			return false
		}
	}
	if held, d := pi.p.pending(); held {
		pi.wait.Reset(d)
	}
	return true
}

// read hands one terminal read to the pane.
func (pi *paneInput) read(b []byte, key func(string) bool, paste func(string)) bool {
	pi.wait.Stop()
	return pi.handle(unbracketed(pi.p.push(b)), key, paste)
}

// unbracketed catches a paste from a terminal without bracketed paste: a burst
// of unbracketedMin or more characters with a line break, all in one read, is
// not someone typing hotkeys (d drops a node), so it becomes one paste instead
// of keys. Shorter bursts stay keys: fast typing can put "ok", Enter and the
// next key in one read, and that Enter must still send.
const unbracketedMin = 8

func unbracketed(events []inputEvent) []inputEvent {
	if len(events) < unbracketedMin {
		return events
	}
	var text strings.Builder
	newline := false
	for _, ev := range events {
		if ev.isPaste || strings.HasPrefix(ev.key, "\x1b") || ev.key == "\x03" {
			return events // a real paste, an arrow or ctrl-c: these are keys
		}
		if ev.key == "\r" || ev.key == "\n" {
			newline = true
		}
		text.WriteString(ev.key)
	}
	if !newline {
		return events
	}
	return []inputEvent{{paste: text.String(), isPaste: true}}
}

// settle is called when the wait timer fires.
func (pi *paneInput) settle(key func(string) bool, paste func(string)) bool {
	return pi.handle(pi.p.flush(), key, paste)
}

// children are commands a pane started in the background (talk, sync, ...);
// the pane waits for them before it exits, so none is cut off half way.
var children sync.WaitGroup

func waitChildren(limit time.Duration) {
	done := make(chan struct{})
	go func() { children.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// cleanGraph strips terminal control characters from everything a pane shows
// that someone else wrote (titles, outcomes, logs, names), so an agent's text
// can never steer the terminal (set the clipboard, rewrite the screen).
// Whole lines are cleaned again by termSafe when drawn, which also covers ids.
func cleanGraph(g *Graph) {
	for _, n := range g.Nodes {
		n.Title, n.Outcome, n.Agent, n.Body = clean(n.Title), clean(n.Outcome), clean(n.Agent), clean(n.Body)
		n.Mode, n.Check, n.Status = clean(n.Mode), clean(n.Check), clean(n.Status)
		for _, list := range [][]string{n.Decisions, n.Touches, n.Stale} {
			for i := range list {
				list[i] = clean(list[i])
			}
		}
	}
	for _, f := range g.Info {
		f.Label = clean(f.Label)
	}
}

// termSafe makes one drawn line safe for the terminal: the colour codes the
// pane itself writes (ESC [ digits ; m) stay, every other escape sequence and
// control character goes. Everything a pane prints passes through it, so text
// from a node file (an id, a check's output, a touched path) can never set the
// clipboard, retitle the window or move the cursor.
func termSafe(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		c := line[i]
		if c == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] == ';' || (line[j] >= '0' && line[j] <= '9')) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				b.WriteString(line[i : j+1])
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case !unicode.IsControl(r) && r != 0x2028 && r != 0x2029:
			b.WriteString(line[i : i+size])
		}
		i += size
	}
	return b.String()
}

// runeWidth is how many terminal columns r takes: 2 for wide East Asian
// characters and most emoji, 0 for combining marks, 1 otherwise. A cheap
// table, enough to keep CJK titles and emoji from breaking a pane's layout.
func runeWidth(r rune) int {
	switch {
	case r == 0 || r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFE0F || r == 0xFE0E:
		return 0
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF, // CJK radicals, kana, symbols
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, // CJK ideographs
		r >= 0xA000 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3, // Yi, Hangul
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, // compatibility ideographs and forms
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6, // fullwidth forms
		r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F680 && r <= 0x1F6FF, // emoji, transport
		r >= 0x1F900 && r <= 0x1F9FF, r >= 0x1FA70 && r <= 0x1FAFF, // more emoji
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions
		return 2
	}
	return 1
}

// safeLines joins a pane's lines for drawing, each passed through termSafe.
func safeLines(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = termSafe(l)
	}
	return strings.Join(out, "\r\n")
}

// paneSignals end a pane cleanly (screen and terminal restored). SIGQUIT
// (ctrl-\) is among them: Go's default for it exits without restoring.
var paneSignals = []os.Signal{syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// textWidth is how many columns s takes on screen.
func textWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// report shows a background command's last line without ever blocking (the
// pane may already be closing) and without terminal control characters.
func report(ch chan string, msg string) {
	select {
	case ch <- clean(msg):
	default:
	}
}
