package main

// `ct watch`: a plain, live view of the tree for a pane beside your agent.
// Arrow keys move, single keys act. Every action runs a normal ct command as
// the human, so the agents involved hear about it through intercom.

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRev    = "\x1b[7m"
	cOrange = "\x1b[38;5;208m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cBlue   = "\x1b[34m"
)

type wrow struct {
	id     string // "" for a heading
	text   string
	indent int
}

type watcher struct {
	tree       string
	g          *Graph
	view       string // next | tree
	rows       []wrow
	sel        string
	top        int
	w, h       int
	msg        string
	sig        string
	inputOn    bool
	prompt     string
	input      []rune
	onSubmit   func(string)
	overlay    []string
	results    chan string
	stty       string
	humanMails int
}

func cmdWatch(a *Args) {
	tree := requireTree()
	sc := exec.Command("stty", "-g")
	sc.Stdin = os.Stdin
	st, err := sc.Output()
	if err != nil {
		die("ct watch needs a terminal")
	}
	w := &watcher{tree: tree, view: "next", results: make(chan string, 8), stty: strings.TrimSpace(string(st))}
	w.setup()
	defer w.restore()

	keys := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- append([]byte{}, buf[:n]...)
		}
	}()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	tick := time.NewTicker(800 * time.Millisecond)
	defer tick.Stop()

	w.size()
	w.reload()
	w.render()
	for {
		select {
		case k, ok := <-keys:
			if !ok {
				return
			}
			for _, one := range splitKeys(k) {
				if !w.key([]byte(one)) {
					return
				}
			}
		case s := <-sigs:
			if s != syscall.SIGWINCH {
				return
			}
			w.size()
		case r := <-w.results:
			w.msg = r
			w.reload()
		case <-tick.C:
			if sig := w.signature(); sig != w.sig {
				w.reload()
			} else {
				continue
			}
		}
		w.render()
	}
}

func stty(args ...string) {
	c := exec.Command("stty", args...)
	c.Stdin = os.Stdin
	_ = c.Run()
}

func (w *watcher) setup() {
	stty("-icanon", "-echo", "min", "1")
	fmt.Print("\x1b[?1049h\x1b[?25l")
}

func (w *watcher) restore() {
	fmt.Print("\x1b[?25h\x1b[?1049l")
	stty(w.stty)
}

func (w *watcher) size() {
	c := exec.Command("stty", "size")
	c.Stdin = os.Stdin
	out, err := c.Output()
	w.h, w.w = 24, 80
	if err == nil {
		fmt.Sscan(string(out), &w.h, &w.w)
	}
}

// signature changes whenever any node file, or the human's inbox, changes.
func (w *watcher) signature() string {
	var b strings.Builder
	entries, _ := os.ReadDir(filepath.Join(w.tree, "nodes"))
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s%d%d;", e.Name(), info.ModTime().UnixNano(), info.Size())
		}
	}
	fmt.Fprintf(&b, "|%d", w.unreadHuman())
	return b.String()
}

func (w *watcher) unreadHuman() int {
	if icBin() == "" {
		return 0
	}
	out, err := exec.Command(icBin(), "inbox", "--peek").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n"))
}

func (w *watcher) reload() {
	w.g = load(w.tree)
	w.sig = w.signature()
	w.humanMails = w.unreadHuman()
	w.rows = nil
	g := w.g
	if w.view == "next" {
		groups := []struct{ state, name string }{
			{"needs-you", "Needs you"}, {"proposed", "Proposed"}, {"stale", "Stale"},
			{"ready", "Ready for review"}, {"working", "Working"}, {"open", "Open"},
		}
		for _, gr := range groups {
			var ids []string
			for _, i := range g.IDs {
				if g.Info[i].State == gr.state {
					ids = append(ids, i)
				}
			}
			if len(ids) == 0 {
				continue
			}
			g.byCreated(ids)
			w.rows = append(w.rows, wrow{text: fmt.Sprintf("%s · %d", gr.name, len(ids))})
			for _, i := range ids {
				w.rows = append(w.rows, wrow{id: i, indent: 1})
			}
		}
	} else {
		for _, h := range g.heads() {
			if resolved(g.Nodes[h].Status) {
				continue
			}
			seen := map[string]bool{}
			var walk func(i string, d int)
			walk = func(i string, d int) {
				if seen[i] {
					return
				}
				seen[i] = true
				w.rows = append(w.rows, wrow{id: i, indent: d})
				for _, c := range g.Info[i].Children {
					if !resolved(g.Info[c].State) {
						walk(c, d+1)
					}
				}
			}
			walk(h, 0)
		}
	}
	if w.index(w.sel) < 0 {
		w.sel = ""
		for _, r := range w.rows {
			if r.id != "" {
				w.sel = r.id
				break
			}
		}
	}
}

func (w *watcher) index(id string) int {
	for k, r := range w.rows {
		if r.id != "" && r.id == id {
			return k
		}
	}
	return -1
}

func (w *watcher) move(d int) {
	k := w.index(w.sel)
	for j := k + d; j >= 0 && j < len(w.rows); j += d {
		if w.rows[j].id != "" {
			w.sel = w.rows[j].id
			return
		}
	}
}

// ---------------------------------------------------------------- keys

func (w *watcher) key(b []byte) bool {
	s := string(b)
	if w.inputOn {
		switch {
		case s == "\x03":
			return false
		case s == "\x1b":
			w.inputOn, w.msg = false, "cancelled"
		case s == "\r" || s == "\n":
			w.inputOn = false
			w.onSubmit(strings.TrimSpace(string(w.input)))
		case s == "\x7f" || s == "\b":
			if len(w.input) > 0 {
				w.input = w.input[:len(w.input)-1]
			}
		case strings.HasPrefix(s, "\x1b"):
		default:
			for _, r := range strings.ReplaceAll(s, "\n", " ") {
				if r >= 32 {
					w.input = append(w.input, r)
				}
			}
		}
		return true
	}
	if s == "q" || s == "\x03" {
		return false
	}
	if w.overlay != nil {
		w.overlay = nil
		return true
	}
	if s == "\x1b" {
		return false
	}
	id := w.sel
	state := ""
	if id != "" {
		state = w.g.Info[id].State
	}
	switch s {
	case "q", "\x03":
		return false
	case "\x1b[A", "k":
		w.move(-1)
	case "\x1b[B", "j":
		w.move(1)
	case "\t":
		if w.view == "next" {
			w.view = "tree"
		} else {
			w.view = "next"
		}
		w.reload()
	case "r":
		w.reload()
		w.msg = "reloaded"
	case "s":
		w.do("syncing…", "sync")
	case "?":
		w.overlay = helpLines()
	case "m":
		out, _ := exec.Command(icBin(), "inbox").CombinedOutput()
		w.overlay = strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	case "\r", "\n":
		if id != "" {
			w.overlay = strings.Split(strings.TrimRight(w.g.showText(id), "\n"), "\n")
		}
	}
	if id == "" {
		return true
	}
	switch s {
	case "a":
		w.ask("answer "+id, func(t string) { w.act("answer", id, t) })
	case "y":
		switch state {
		case "proposed":
			w.act("accept", id, "")
		case "ready":
			w.act("done", id, "")
		case "stale":
			w.act("ack", id, "")
		default:
			w.msg = "y accepts a proposed, ready or stale node"
		}
	case "x":
		switch state {
		case "proposed":
			w.ask("reject "+id+" (reason, optional)", func(t string) { w.act("reject", id, t) })
		case "ready", "stale", "done":
			w.ask("send "+id+" back (why)", func(t string) { w.act("open", id, t) })
		default:
			w.msg = "x rejects a proposal or sends a node back"
		}
	case "c":
		w.ask("decision for "+id+" and everything below", func(t string) { w.act("decide", id, t) })
	case "+":
		w.ask("new sub-task under "+id, func(t string) { w.act("add", id, t) })
	case "e":
		w.ask("note on "+id, func(t string) { w.act("note", id, t) })
	case "t":
		w.ask("talk about "+id, func(t string) { w.bg("talk", id, t) })
	case "w":
		w.bg("work", id, "")
	case "n":
		w.bg("run", id, "")
	case "d":
		w.act("drop", id, "")
	}
	return true
}

func (w *watcher) ask(prompt string, then func(string)) {
	w.inputOn, w.prompt, w.input = true, prompt, nil
	w.onSubmit = func(t string) {
		if t == "" && !strings.Contains(prompt, "optional") {
			w.msg = "nothing sent"
			return
		}
		then(t)
	}
}

// act runs a quick ct command as the human and shows its result.
func (w *watcher) act(action, id, text string) {
	args, ok := actionArgs(action, id, text)
	if !ok {
		w.msg = "can't " + action + " without text"
		return
	}
	out, _ := runSelf(w.tree, args)
	w.msg = lastLines(out, 1)
	w.reload()
}

// bg runs a slower ct command (talking, starting agents) without freezing the view.
func (w *watcher) bg(action, id, text string) {
	args, ok := actionArgs(action, id, text)
	if !ok {
		w.msg = "can't " + action + " without text"
		return
	}
	w.msg = action + "…"
	go func() {
		out, _ := runSelf(w.tree, args)
		w.results <- lastLines(out, 1)
	}()
}

func (w *watcher) do(msg string, args ...string) {
	w.msg = msg
	go func() {
		out, _ := runSelf(w.tree, args)
		w.results <- lastLines(out, 1)
	}()
}

func helpLines() []string {
	return []string{
		"ct watch · keys", "",
		"  ↑ ↓ / j k   move                 tab   switch Next / Tree",
		"  enter       full node             m     your intercom inbox",
		"  a           answer (needs you)    c     add a decision",
		"  y           accept / done / still holds",
		"  x           reject / send back",
		"  t           talk to the agent about this node",
		"  w           ask the agent in this tab to work on it",
		"  n           start a new agent on it (herdr tab)",
		"  +  e  d     add sub-task, note, drop",
		"  s  r  q     sync with code, reload, quit", "",
		"any key closes this",
	}
}

// ---------------------------------------------------------------- drawing

func visible(s string) int { return utf8.RuneCountInString(s) }

func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > n {
		if n == 1 {
			return "…"
		}
		return string(r[:n-1]) + "…"
	}
	return s
}

func stateColor(state string) string {
	switch state {
	case "needs-you", "proposed", "ready":
		return cOrange
	case "stale":
		return cRed
	case "working":
		return cBlue
	case "done", "dropped":
		return cGreen
	}
	return ""
}

func (w *watcher) render() {
	var out []string
	add := func(color, plain string) {
		out = append(out, color+cut(plain, w.w)+cReset)
	}
	g := w.g
	count := func(st string) int {
		n := 0
		for _, i := range g.IDs {
			if g.Info[i].State == st {
				n++
			}
		}
		return n
	}
	project := filepath.Base(filepath.Dir(w.tree))
	tabs := "[Next]  Tree"
	if w.view == "tree" {
		tabs = " Next  [Tree]"
	}
	head := fmt.Sprintf(" ct · %s   %s   %d need you · %d proposed · %d ready · %d working", project, tabs,
		count("needs-you"), count("proposed"), count("ready"), count("working"))
	if w.humanMails > 0 {
		head += fmt.Sprintf("   ✉ %d (m)", w.humanMails)
	}
	add(cBold, head)
	add(cDim, strings.Repeat("─", w.w))

	if w.overlay != nil {
		for _, l := range w.overlay {
			if len(out) >= w.h-1 {
				break
			}
			add("", " "+l)
		}
		w.flush(out)
		return
	}

	detail := w.detail()
	listH := w.h - len(out) - len(detail) - 3
	if listH < 3 {
		listH = 3
	}
	k := w.index(w.sel)
	if k < w.top {
		w.top = k
	}
	if k >= w.top+listH {
		w.top = k - listH + 1
	}
	if w.top < 0 {
		w.top = 0
	}
	for j := w.top; j < len(w.rows) && j < w.top+listH; j++ {
		r := w.rows[j]
		if r.id == "" {
			add(cDim+cBold, " "+r.text)
			continue
		}
		n, f := g.Nodes[r.id], g.Info[r.id]
		line := fmt.Sprintf(" %s%-11s %s", strings.Repeat("  ", r.indent), f.Label, n.Title)
		if f.Unblocks > 0 && !resolved(f.State) {
			line += fmt.Sprintf("  · unblocks %d", f.Unblocks)
		}
		if n.Agent != "" && (f.State == "working" || f.State == "needs-you") {
			line += "  · " + n.Agent
		}
		color := stateColor(f.State)
		if r.id == w.sel {
			color = cRev + color
			line = cut(line, w.w)
			line += strings.Repeat(" ", max(0, w.w-visible(line)))
		}
		add(color, line)
	}
	if len(w.rows) == 0 {
		add(cDim, " nothing here. tab switches view; ct add \"<goal>\" starts a tree")
	}
	for len(out) < w.h-len(detail)-2 {
		out = append(out, "")
	}
	add(cDim, strings.Repeat("─", w.w))
	for _, l := range detail {
		add("", l)
	}
	if w.inputOn {
		add(cOrange, " "+w.prompt+" › "+string(w.input)+"▏")
	} else {
		add(cDim, " "+w.keysFor()+"   "+w.msg)
	}
	w.flush(out)
}

func (w *watcher) flush(lines []string) {
	if len(lines) > w.h {
		lines = lines[:w.h]
	}
	fmt.Print("\x1b[H\x1b[2J" + strings.Join(lines, "\r\n"))
}

func (w *watcher) detail() []string {
	if w.sel == "" {
		return nil
	}
	g, i := w.g, w.sel
	n, f := g.Nodes[i], g.Info[i]
	var d []string
	d = append(d, fmt.Sprintf(" %s%s%s  %s · %s", cBold, n.Title, cReset, f.Label, i))
	if q := lastLogText(n, "QUESTION: "); q != "" && f.State == "needs-you" {
		d = append(d, " Q: "+q)
	}
	if n.Outcome != "" {
		d = append(d, " outcome: "+n.Outcome)
	}
	if len(n.Stale) > 0 {
		d = append(d, " changed since resolved: "+strings.Join(n.Stale, ", "))
	}
	for k, dc := range g.decisionsInForce(i) {
		if k == 2 {
			break
		}
		from := ""
		if dc[0] != i {
			from = " (from " + dc[0] + ")"
		}
		d = append(d, " decision: "+dc[1]+from)
	}
	if last := lastLogText(n, ""); last != "" {
		d = append(d, cDim+" last: "+last+cReset)
	}
	return d
}

// lastLogText returns the text after prefix in the last log line containing it.
func lastLogText(n *Node, prefix string) string {
	text := ""
	for _, l := range strings.Split(n.Body, "\n") {
		if m := logLine.FindStringSubmatch(l); m != nil {
			if k := strings.Index(m[2], prefix); k >= 0 {
				text = "[" + m[1] + "] " + m[2]
				if prefix != "" {
					text = m[2][k+len(prefix):]
				}
			}
		}
	}
	return text
}

func (w *watcher) keysFor() string {
	if w.sel == "" {
		return "tab view · ? help · q quit"
	}
	switch w.g.Info[w.sel].State {
	case "needs-you":
		return "a answer · t talk · c decide · ? help"
	case "proposed":
		return "y accept · x reject · t talk · ? help"
	case "ready":
		return "y accept as done · x send back · t talk · ? help"
	case "stale":
		return "y still holds · x reopen · t talk · ? help"
	case "open":
		return "w work on it · n new agent · t talk · + sub-task · ? help"
	case "working":
		return "t talk · c decide · e note · ? help"
	}
	return "t talk · c decide · + sub-task · ? help"
}

// splitKeys breaks one read into separate keys: escape sequences (arrows)
// stay whole, everything else is one rune each, so fast typing never merges keys.
func splitKeys(b []byte) []string {
	var keys []string
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+2 < len(b) && b[i+1] == '[' {
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) {
				j++
			}
			keys = append(keys, string(b[i:j]))
			i = j
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			size = 1
		}
		keys = append(keys, string(b[i:i+size]))
		i += size
	}
	return keys
}
