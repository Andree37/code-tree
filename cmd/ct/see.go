package main

// `ct see`: the conversation graph of the package Claude is working in, drawn
// as boxes and lines in a terminal pane. It follows the Claude session in the
// same herdr tab. Move between nodes with the arrows; `t` talks to Claude about
// the selected thread and `s` switches Claude to it, both through intercom.

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	boxW   = 34 // outer width of a card, its half-block frame included
	boxH   = 5  // outer height
	slotW  = boxW + 3
	layerH = boxH + 3 // three rows between layers: a stem, the rail, then the arrow
)

type cell struct {
	r     rune
	color string
}

type seer struct {
	session  string // Claude session id being followed
	auto     bool   // no --session given: follow whichever Claude is in this tab now
	st       convoState
	root     string
	pkgs     []string
	pkg      string // package shown
	pinned   bool   // the human picked a package; stop following
	ox, oy   int    // scroll position of the graph
	tree     string
	g        *Graph
	head     string
	sel      string
	pos      map[string][2]int // node → x, y on the canvas
	canvas   [][]cell
	w, h     int
	msg      string
	sig      string
	overlay  []string
	inputOn  bool
	prompt   string
	input    []rune
	submit   func(string)
	stty     string
	results  chan string
	serve    *exec.Cmd
	pickOn   bool      // the package list is open
	pickSel  int       // which package is highlighted in it
	pickRows []pkgInfo // what the list shows
	lastSeen string
	ended    bool // the Claude in this tab has exited
}

func cmdSee(a *Args) {
	id := pickSession(a.Get("session", ""))
	if id == "" {
		die("no Claude session with a code-tree yet. Run `ct install claude`, then talk to Claude for a turn.")
	}
	sc := exec.Command("stty", "-g")
	sc.Stdin = os.Stdin
	saved, err := sc.Output()
	if err != nil {
		die("ct see needs a terminal")
	}
	s := &seer{session: id, auto: a.Get("session", "") == "", stty: strings.TrimSpace(string(saved)), results: make(chan string, 8)}
	stty("-icanon", "-echo", "min", "1")
	fmt.Print("\x1b[?1049h\x1b[?25l\x1b[?2004h") // alternate screen, no cursor, bracketed paste
	defer func() {
		waitChildren(5 * time.Second) // a message being sent finishes first
		if s.serve != nil && s.serve.Process != nil {
			_ = s.serve.Process.Kill()
		}
		fmt.Print("\x1b[?2004l\x1b[?25h\x1b[?1049l")
		stty(s.stty)
		flushInput(os.Stdin.Fd()) // keys typed for the pane never reach the shell
	}()

	keys := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
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
	signal.Notify(sigs, paneSignals...)
	tick := time.NewTicker(800 * time.Millisecond)
	defer tick.Stop()

	in := newPaneInput()
	s.size()
	s.reload()
	s.render()
	for {
		select {
		case k, ok := <-keys:
			if !ok {
				return
			}
			if !in.read(k, s.key, s.paste) {
				return
			}
		case <-in.wait.C:
			if !in.settle(s.key, s.paste) {
				return
			}
		case sg := <-sigs:
			if sg != syscall.SIGWINCH {
				return
			}
			s.size()
		case r := <-s.results:
			s.msg = r
		case <-tick.C:
			if s.signature() == s.sig {
				continue
			}
			s.reload()
		}
		s.render()
	}
}

// pickSession: the given one, else the Claude session in this herdr tab, else
// the session whose tree changed last.
func pickSession(want string) string {
	entries, _ := os.ReadDir(filepath.Join(convoHome(), "sessions"))
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	if want != "" {
		for _, id := range ids {
			if id == want || strings.HasPrefix(id, want) || strings.EqualFold(icSession(id)["name"], want) {
				return id
			}
		}
		return ""
	}
	if tab := os.Getenv("HERDR_TAB_ID"); tab != "" {
		if id, _ := tabSession(tab); id != "" {
			return id
		}
	}
	return latestSession(ids)
}

// tabSession is the Claude session in a herdr tab: the live one seen last, or,
// when none is live, the one that ended last (ended is then true). Inside herdr
// the pane only ever follows its own tab, never another tab's Claude.
func tabSession(tab string) (id string, ended bool) {
	live, liveSeen, gone, goneSeen := "", "", "", ""
	icEntries, _ := os.ReadDir(filepath.Join(icHome(), "sessions"))
	for _, e := range icEntries {
		kv := icSession(e.Name())
		if kv["tab"] != tab || kv["kind"] != "claude" {
			continue
		}
		if kv["ended"] == "" && kv["seen"] > liveSeen {
			live, liveSeen = kv["id"], kv["seen"]
		}
		if kv["ended"] != "" && kv["seen"] > goneSeen {
			gone, goneSeen = kv["id"], kv["seen"]
		}
	}
	if live != "" {
		return live, false
	}
	return gone, gone != ""
}

// latestSession is the session whose tree changed last.
func latestSession(ids []string) string {
	best, when := "", ""
	for _, id := range ids {
		if u := readConvo(id)["updated"]; u > when {
			best, when = id, u
		}
	}
	return best
}

func (s *seer) size() {
	c := exec.Command("stty", "size")
	c.Stdin = os.Stdin
	s.h, s.w = 24, 80
	if out, err := c.Output(); err == nil {
		fmt.Sscan(string(out), &s.h, &s.w)
	}
}

func (s *seer) signature() string {
	var b strings.Builder
	if s.auto {
		id, ended := s.follow()
		fmt.Fprintf(&b, "%s%v;", id, ended) // a new session in this tab, or its Claude ended
	}
	if info, err := os.Stat(filepath.Join(sessionDir(s.session), "convo")); err == nil {
		fmt.Fprintf(&b, "%d;", info.ModTime().UnixNano())
	}
	if s.tree != "" {
		if info, err := os.Stat(filepath.Join(s.tree, "GRAPH.md")); err == nil {
			fmt.Fprintf(&b, "%d;", info.ModTime().UnixNano())
		}
	}
	return b.String()
}

// follow is the session the pane should show now. Inside herdr that is this
// tab's Claude, even after it ended (so t and g never reach another tab's
// Claude); outside herdr, the session whose tree changed last.
func (s *seer) follow() (id string, ended bool) {
	if tab := os.Getenv("HERDR_TAB_ID"); tab != "" {
		id, ended = tabSession(tab)
		if id == "" { // no Claude in this tab yet: stay on what the pane started with
			return s.session, false
		}
		return id, ended
	}
	return pickSession(""), false
}

func (s *seer) reload() {
	if s.auto { // after /clear or /resume the tab runs a new session: follow it
		id, ended := s.follow()
		if id != "" && id != s.session {
			s.session, s.head, s.sel, s.lastSeen = id, "", "", ""
		}
		s.ended = ended
	}
	s.sig = s.signature()
	s.st = readConvo(s.session)
	s.root = s.st["repo"]
	if s.root == "" {
		s.root = repoRoot(s.st["cwd"])
	}
	s.pkgs = repoPackages(s.root)
	if !s.pinned || s.pkg == "" {
		if p, ok := s.st["package"]; ok {
			s.pkg = p
		} else if len(s.pkgs) > 0 {
			s.pkg = s.pkgs[0]
		}
	}
	s.tree = pkgTree(s.root, s.pkg)
	s.g = load(s.tree)
	cleanGraph(s.g) // what agents wrote never reaches the terminal raw
	heads := s.g.heads()
	cur := s.st["current"]
	// follow Claude: show the topic it is in, and select its thread when it moves
	if !s.pinned && cur != "" && s.g.Nodes[cur] != nil {
		if hs := s.g.headsOf(cur); len(hs) > 0 {
			s.head = hs[0]
		}
		if cur != s.lastSeen {
			s.sel, s.lastSeen = cur, cur
		}
	}
	if s.g.Nodes[s.head] == nil { // e.g. a subject from the folder shown before
		s.head = ""
		if len(heads) > 0 {
			s.head = heads[len(heads)-1]
		}
	}
	if s.g.Nodes[s.sel] == nil || (s.head != "" && !inSubtree(s.g, s.head, s.sel)) {
		s.sel = s.head
	}
	s.layout()
}

func inSubtree(g *Graph, head, id string) bool {
	return id == head || g.descendants(head)[id]
}

// ---------------------------------------------------------------- layout and drawing

// layout places the selected topic as a tidy tree: leaves side by side,
// each parent centred over its children.
func (s *seer) layout() {
	s.pos = map[string][2]int{}
	if s.head == "" {
		s.canvas = nil
		return
	}
	g := s.g
	next := 0
	maxDepth := 0
	var place func(id string, depth int, seen map[string]bool) int
	place = func(id string, depth int, seen map[string]bool) int {
		seen[id] = true
		if depth > maxDepth {
			maxDepth = depth
		}
		var xs []int
		for _, c := range g.Info[id].Children {
			if !seen[c] {
				xs = append(xs, place(c, depth+1, seen))
			}
		}
		x := 0
		if len(xs) == 0 {
			x = next * slotW
			next++
		} else {
			x = (xs[0] + xs[len(xs)-1]) / 2
		}
		s.pos[id] = [2]int{x, depth * layerH}
		return x
	}
	place(s.head, 0, map[string]bool{})
	W, H := next*slotW, (maxDepth+1)*layerH
	s.canvas = make([][]cell, H)
	for y := range s.canvas {
		s.canvas[y] = make([]cell, W)
		for x := range s.canvas[y] {
			s.canvas[y][x] = cell{r: ' '}
		}
	}
	for id := range s.pos {
		s.drawBox(id)
	}
	for id := range s.pos { // after the boxes: lines join into their borders
		s.drawEdges(id)
	}
}

func (s *seer) put(x, y int, r rune, color string) {
	if r != 0 && unicode.IsControl(r) {
		r = ' '
	}
	if y >= 0 && y < len(s.canvas) && x >= 0 && x < len(s.canvas[y]) {
		s.canvas[y][x] = cell{r, color}
	}
}

// text writes str from column x. A wide character takes two cells: itself and
// a 0 placeholder that render skips; zero-width characters are left out.
func (s *seer) text(x, y int, str, color string) {
	for _, r := range str {
		switch runeWidth(r) {
		case 0:
		case 2:
			s.put(x, y, r, color)
			s.put(x+1, y, 0, color)
			x += 2
		default:
			s.put(x, y, r, color)
			x++
		}
	}
}

func hot(state string) bool { return state == "needs-you" || state == "ready" }

// drawEdges joins a card to its points: down from the card, along a rounded
// rail, and an arrow into each point's card.
func (s *seer) drawEdges(id string) {
	var kids []string
	for _, c := range s.g.Info[id].Children {
		if _, ok := s.pos[c]; ok {
			kids = append(kids, c)
		}
	}
	if len(kids) == 0 {
		return
	}
	p := s.pos[id]
	pc, y := p[0]+boxW/2, p[1]+boxH+1 // y: the rail; the stem is the row above it
	minc, maxc := pc, pc
	for _, c := range kids {
		cc := s.pos[c][0] + boxW/2
		minc, maxc = min(minc, cc), max(maxc, cc)
	}
	edgeCol := func(c string) string {
		if s.g.Info[c].State == "needs-you" {
			return fg(colEdgeHot)
		}
		return fg(colEdge)
	}
	rail := fg(colEdge)
	for _, c := range kids {
		if s.g.Info[c].State == "needs-you" {
			rail = fg(colEdgeHot)
		}
	}
	s.put(pc, y-1, '│', rail)
	for x := minc; x <= maxc; x++ {
		s.put(x, y, '─', rail)
	}
	for _, c := range kids {
		cc := s.pos[c][0] + boxW/2
		col := edgeCol(c)
		s.put(cc, y+1, '▼', col)
		switch {
		case minc == maxc:
			s.put(cc, y, '│', col)
		case cc == minc:
			s.put(cc, y, '╭', col)
		case cc == maxc:
			s.put(cc, y, '╮', col)
		default:
			s.put(cc, y, '┬', col)
		}
	}
	if minc != maxc {
		j := '┴'
		switch pc {
		case minc:
			j = '╰'
		case maxc:
			j = '╯'
		}
		for _, c := range kids {
			if s.pos[c][0]+boxW/2 == pc {
				j = '┼'
				if pc == minc {
					j = '├'
				} else if pc == maxc {
					j = '┤'
				}
			}
		}
		s.put(pc, y, j, rail)
	}
}

func wrapTitle(t string, width, lines int) []string {
	var out []string
	cur := ""
	for _, word := range strings.Fields(t) {
		if visible(cur)+visible(word)+1 > width && cur != "" {
			out = append(out, cur)
			cur = ""
		}
		if cur != "" {
			cur += " "
		}
		cur += word
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) > lines {
		out = out[:lines]
		out[lines-1] = cut(out[lines-1]+" …", width)
	}
	for i := range out {
		out[i] = cut(out[i], width)
	}
	return out
}

// drawBox draws one card: a filled block whose half-cell frame takes the
// card's own colour, yellow for the card Claude is on, white for the one you
// picked. Inside: a dot when Claude is here, the title, "+" to branch off it,
// and its badges.
func (s *seer) drawBox(id string) {
	n, f := s.g.Nodes[id], s.g.Info[id]
	p := s.pos[id]
	x, y := p[0], p[1]
	st := styleFor(f)
	here := id == s.st["current"]
	frame := st.fill
	switch {
	case id == s.sel:
		frame = colSelected
	case here:
		frame = colGlow
	}
	fill := bg(st.fill)
	fr := fg(frame)
	right, bottom := x+boxW-1, y+boxH-1
	s.put(x, y, '▗', fr)
	s.put(right, y, '▖', fr)
	s.put(x, bottom, '▝', fr)
	s.put(right, bottom, '▘', fr)
	for i := x + 1; i < right; i++ {
		s.put(i, y, '▄', fr)
		s.put(i, bottom, '▀', fr)
	}
	inner := boxW - 2
	for j := y + 1; j < bottom; j++ {
		s.put(x, j, '▐', fr) // the half next to the fill is the frame, the outer half stays clear
		s.put(right, j, '▌', fr)
		s.text(x+1, j, strings.Repeat(" ", inner), fill)
	}

	text := fill + fg(st.text)
	if st.bold || id == s.sel {
		text += cBold
	}
	if here {
		s.text(x+2, y+1, "●", fill+fg(colGlow))
	}
	s.text(right-3, y+1, " + ", bg(st.chip)+fg(st.text))
	lines := wrapTitle(n.Title, inner-6, 2) // room for the dot on the left and + on the right
	for k, line := range lines {
		pad := (inner - visible(line)) / 2
		s.text(x+1+pad, y+1+k, line, text)
	}

	cx := x + 2
	for _, c := range s.badges(id).chips(st) {
		if cx+visible(c[0]) > right-1 {
			break
		}
		s.text(cx, y+3, c[0], c[1])
		cx += visible(c[0]) + 1
	}
	if word := cardWord(f); word != "" && cx+visible(word) < right-1 {
		s.text(right-1-visible(word), y+3, word, fill+fg(colMuted))
	}
}

// cardWord is the short state shown in a card's corner, for states the colour
// alone does not say well.
func cardWord(f *Info) string {
	switch {
	case f.Kind == "head":
		return "subject"
	case f.State == "needs-you":
		return "for you"
	case f.State == "proposed":
		return "proposed"
	case f.State == "stale":
		return "stale"
	case f.State == "dropped":
		return "dropped"
	}
	return ""
}

func (s *seer) render() {
	var out []string
	add := func(color, plain string) { out = append(out, color+cut(plain, s.w)+cReset) }

	s.layout() // the selection is drawn into the boxes, so redraw on every key

	state := "Claude is idle"
	if s.st["busy"] != "" {
		state = "Claude is working…"
	}
	if s.ended {
		state = "Claude in this tab has ended"
	}
	head := fmt.Sprintf(" What you and Claude discussed in %s/", pkgName(s.root, s.pkg))
	if heads := s.g.heads(); len(heads) > 1 {
		for i, h := range heads {
			if h == s.head {
				head += fmt.Sprintf("   subject %d of %d (o: next)", i+1, len(heads))
			}
		}
	}
	right := state
	pad := s.w - visible(head) - visible(right) - 1
	if pad < 1 {
		pad = 1
	}
	add(cBold, head+strings.Repeat(" ", pad)+right)
	if visible(stripSGR(legend())) <= s.w {
		out = append(out, legend()+cReset)
	} else {
		add(cDim, strings.Repeat("─", s.w))
	}

	if s.overlay != nil {
		for _, l := range s.overlay {
			if len(out) < s.h-1 {
				add("", " "+l)
			}
		}
		s.flush(out)
		return
	}

	graphH := max(0, s.h-len(out)-6) // a tiny pane shows no graph rather than crash
	graphW := s.w
	var panel []string
	if s.pickOn { // the package list takes the left side; the graph keeps the rest
		pw := min(40, max(28, s.w/3))
		graphW = s.w - pw - 1
		panel = s.pickerLines(pw, graphH)
	}
	var rows []string
	if s.canvas == nil {
		rows = append(rows, cDim+cut(" Nothing here yet. The tree grows as you and Claude talk about this package.", graphW)+cReset)
	} else {
		// scroll only as far as needed to keep the picked box in view
		sp := s.pos[s.sel]
		W, H := len(s.canvas[0]), len(s.canvas)
		if W > graphW {
			if sp[0]-1 < s.ox {
				s.ox = sp[0] - 1
			} else if sp[0]+boxW+1 > s.ox+graphW {
				s.ox = sp[0] + boxW + 1 - graphW
			}
			s.ox = min(max(0, s.ox), W-graphW)
		} else {
			s.ox = -(graphW - W) / 2
		}
		if H > graphH {
			if sp[1] < s.oy {
				s.oy = sp[1]
			} else if sp[1]+boxH > s.oy+graphH {
				s.oy = sp[1] + boxH - graphH
			}
			s.oy = min(max(0, s.oy), H-graphH)
		} else {
			s.oy = 0
		}
		ox, oy := s.ox, s.oy
		for row := 0; row < graphH; row++ {
			y := oy + row
			var b strings.Builder
			last := ""
			for col := 0; col < graphW; col++ {
				x := ox + col
				c := cell{r: ' '}
				if y >= 0 && y < H && x >= 0 && x < W {
					c = s.canvas[y][x]
				}
				if c.color != last {
					b.WriteString(cReset + c.color)
					last = c.color
				}
				switch {
				case c.r == 0 && col > 0: // right half of the wide character just written
				case c.r == 0, runeWidth(c.r) == 2 && col == graphW-1: // half of it is off screen
					b.WriteByte(' ')
				default:
					b.WriteRune(c.r)
				}
			}
			rows = append(rows, b.String()+cReset)
		}
	}
	for len(rows) < graphH {
		rows = append(rows, "")
	}
	for i := 0; i < graphH; i++ {
		if panel != nil {
			out = append(out, panel[i]+cDim+"│"+cReset+rows[i])
		} else {
			out = append(out, rows[i])
		}
	}
	for len(out) < s.h-5 {
		out = append(out, "")
	}
	add(cDim, strings.Repeat("─", s.w))
	if n := s.g.Nodes[s.sel]; n != nil {
		f := s.g.Info[s.sel]
		add(cBold, " "+n.Title+cReset+cDim+"  "+plainMeaning(f))
		lines := wrapTitle(n.Outcome, s.w-2, 2) // two lines, so a question's options stay visible
		for len(lines) < 2 {
			lines = append(lines, "")
		}
		color := ""
		if f.State == "needs-you" {
			color = cOrange
		}
		add(color, " "+lines[0])
		add(color, " "+lines[1])
	} else {
		add("", "")
		add("", "")
		add("", "")
	}
	if s.inputOn {
		add(cOrange, " "+s.prompt+" › "+string(s.input)+"▏")
	} else {
		help := "arrows: pick a card · enter: read it · +: branch a new idea off it · t: ask Claude about it · g: go back to it · p: folders · ?: help · q: close"
		if s.msg != "" { // the message first: the help line is wider than most panes
			add("", " "+s.msg+cDim+"   "+help)
		} else {
			add(cDim, " "+help)
		}
	}
	s.flush(out)
}

func (s *seer) flush(lines []string) {
	if len(lines) > s.h {
		lines = lines[:s.h]
	}
	fmt.Print("\x1b[H\x1b[2J" + safeLines(lines))
}

// ---------------------------------------------------------------- keys

func (s *seer) key(k string) bool {
	if s.inputOn {
		switch {
		case k == "\x03":
			return false
		case k == "\x1b":
			s.inputOn, s.msg = false, "cancelled"
		case k == "\r" || k == "\n":
			s.inputOn = false
			if t := strings.TrimSpace(string(s.input)); t != "" {
				s.submit(t)
			}
		case k == "\x7f" || k == "\b":
			if len(s.input) > 0 {
				s.input = s.input[:len(s.input)-1]
			}
		case strings.HasPrefix(k, "\x1b"):
		default:
			for _, r := range k {
				if r >= 32 {
					s.input = append(s.input, r)
				}
			}
		}
		return true
	}
	if k == "q" || k == "\x03" {
		return false
	}
	if s.overlay != nil {
		s.overlay = nil
		return true
	}
	if s.pickOn {
		switch k {
		case "\x1b[A", "k":
			s.pickSel = max(0, s.pickSel-1)
		case "\x1b[B", "j":
			s.pickSel = max(0, min(len(s.pickRows)-1, s.pickSel+1))
		case "\r", "\n":
			if s.pickSel >= 0 && s.pickSel < len(s.pickRows) {
				p := s.pickRows[s.pickSel]
				// picking Claude's own package means: follow Claude again
				s.pkg, s.pinned, s.head, s.sel = p.rel, !p.current, "", ""
				s.reload()
				s.msg = "showing " + p.name
			}
			s.pickOn = false
		case "\x1b", "p":
			s.pickOn = false
		}
		return true
	}
	switch k { // keys about the picked box need one
	case "\r", "\n", "t", "g", "s", "x", "+", "=":
		if s.g.Nodes[s.sel] == nil {
			s.msg = "nothing to pick yet: the map fills in as you and Claude talk"
			return true
		}
	}
	switch k {
	case "\x1b":
		return false
	case "\x1b[A", "k":
		if ps := s.g.Info[s.sel]; ps != nil && len(ps.Parents) > 0 {
			s.sel = ps.Parents[0]
		}
	case "\x1b[B", "j":
		if f := s.g.Info[s.sel]; f != nil && len(f.Children) > 0 {
			best, bd := "", 1<<30
			for _, c := range f.Children {
				if d := abs(s.pos[c][0] - s.pos[s.sel][0]); d < bd {
					best, bd = c, d
				}
			}
			s.sel = best
		}
	case "\x1b[D", "h":
		s.sideways(-1)
	case "\x1b[C", "l":
		s.sideways(1)
	case "\r", "\n":
		s.details()
	case "t":
		id, title := s.sel, s.g.Nodes[s.sel].Title
		s.ask("talk to "+s.st["agent"]+" about “"+title+"”", func(t string) {
			s.send(fmt.Sprintf("[code-tree · %s · %s · %s]\n%s", pkgName(s.root, s.pkg), id, title, t))
		})
	case "+", "=": // = is + without shift
		parent, title := s.sel, s.g.Nodes[s.sel].Title
		s.ask("new idea branching off “"+title+"”", func(t string) { s.branch(parent, t) })
	case "g", "s":
		n, id := s.g.Nodes[s.sel], s.sel
		if n.Status == "needs-you" { // Claude is waiting on an answer: ask for it, never hand back its own question
			s.ask("your answer to: "+n.Outcome, func(t string) {
				s.send(fmt.Sprintf("[code-tree · %s · %s · %s]\nMy answer to your question (%s): %s", pkgName(s.root, s.pkg), id, n.Title, n.Outcome, t))
			})
			break
		}
		s.send(fmt.Sprintf("[code-tree · %s · %s] Let's go back to the thread “%s” in %s. Where it stands: %s Pick it up from there.",
			pkgName(s.root, s.pkg), id, n.Title, pkgName(s.root, s.pkg), n.Outcome))
	case "o":
		heads := s.g.heads()
		for i, h := range heads {
			if h == s.head {
				s.head = heads[(i+1)%len(heads)]
				break
			}
		}
		s.pinned, s.sel = true, s.head
		s.layout()
	case "p":
		s.openPicker()
	case "f":
		s.pinned = false
		s.reload()
		s.msg = "following " + s.st["agent"] + " again"
	case "x":
		s.toggleWaiting()
	case "b":
		s.browser()
	case "?":
		s.overlay = []string{
			cBold + "What is this?" + cReset,
			"  A map of what you and Claude have talked about while working in this code folder.",
			"  It updates by itself after every Claude reply.",
			"",
			"  The top card is the SUBJECT. Each card under it is one point you went into.",
			"  Blue: still open.  Green: settled.  Amber: Claude asked you something there.",
			"  Yellow frame and ●: what Claude is on now.  White frame: the card you picked.",
			"  Badges: !n points waiting for you below it, ?n still open, Dn decisions made, k/n points settled.",
			"",
			cBold + "Keys" + cReset,
			"  arrows      pick a box (the thick frame is the one picked)",
			"  enter       read what happened in that box",
			"  +           branch a new idea off that card: it appears under it and goes to Claude",
			"  t           type a message to Claude about that box; it lands in your Claude chat",
			"  g           ask Claude to go back to that box and continue it",
			"  x           not really waiting for you? mark it settled (press again to undo)",
			"",
			"  o           show the next subject in this folder (if there are several)",
			"  p           list the code folders you worked in, with their status, and pick one",
			"  f           follow Claude again after o or p",
			"  b           open the same map in your web browser",
			"  q / esc     close",
			"",
			"any key closes this",
		}
	}
	return true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// sideways moves to the nearest box in that direction, preferring the same row.
func (s *seer) sideways(dir int) {
	cur := s.pos[s.sel]
	best, bd := "", 1<<30
	for id, p := range s.pos {
		if id == s.sel {
			continue
		}
		dx := (p[0] - cur[0]) * dir
		if dx <= 0 {
			continue
		}
		if d := dx + 4*abs(p[1]-cur[1]); d < bd {
			best, bd = id, d
		}
	}
	if best != "" {
		s.sel = best
	}
}

func (s *seer) details() {
	n, f := s.g.Nodes[s.sel], s.g.Info[s.sel]
	lines := []string{
		cBold + n.Title + cReset,
		plainLabel(f) + " · " + strings.ToLower(f.Kind) + " in " + pkgName(s.root, s.pkg),
		"",
	}
	if n.Outcome != "" {
		lines = append(lines, "Where it stands:")
		for _, l := range wrapTitle(n.Outcome, s.w-6, 12) {
			lines = append(lines, "  "+l)
		}
		lines = append(lines, "")
	}
	lines = append(lines, "History:")
	for _, l := range strings.Split(n.Body, "\n") {
		if m := logLine.FindStringSubmatch(l); m != nil {
			lines = append(lines, "  "+cut(m[2], s.w-4))
		}
	}
	s.overlay = lines
}

func (s *seer) ask(prompt string, then func(string)) {
	s.inputOn, s.prompt, s.input, s.submit = true, prompt, nil, then
}

// send hands a message to the followed Claude session through intercom.
func (s *seer) send(text string) {
	bin := icBin()
	if bin == "" {
		s.msg = "needs intercom: " + icInstall
		return
	}
	if s.ended {
		s.msg = "the Claude in this tab has ended: start it again to talk to it"
		return
	}
	s.msg = "sending…"
	session, results := s.session, s.results // read here: reload may change s.session meanwhile
	children.Add(1)
	go func() {
		defer children.Done()
		cmd := exec.Command(bin, "send", "--topic", "code-tree", "--", session, text)
		cmd.Env = append(os.Environ(), "IC_ID=", "CLAUDECODE=")
		out, _ := cmd.CombinedOutput()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		report(results, strings.TrimPrefix(lines[len(lines)-1], "ic: "))
	}()
}

// browser serves this package's tree with ct's web graph and opens it.
func (s *seer) browser() {
	if s.serve != nil {
		s.msg = "already open in the browser"
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	tree := s.tree
	if abs, err := filepath.Abs(tree); err == nil {
		tree = abs
	}
	cmd := exec.Command(exe, "serve", "--port", "0")
	cmd.Env = append(os.Environ(), "CT_TREE="+tree)
	if err := cmd.Start(); err != nil {
		s.msg = err.Error()
		return
	}
	s.serve = cmd
	s.msg = "opened in the browser"
}

const cWhite = "\x1b[97m"

// plainLabel is a box's state in everyday words.
func plainLabel(f *Info) string {
	if f.Kind == "head" && f.State == "done" {
		return "All settled"
	}
	switch f.State {
	case "needs-you":
		return "Waiting for you"
	case "ready":
		return "Settled"
	case "done":
		return "Settled"
	case "open", "working":
		return "In progress"
	case "waiting":
		var done, all int
		fmt.Sscanf(f.Label, "Waiting %d/%d", &done, &all)
		return fmt.Sprintf("%d of %d settled", done, all)
	}
	return f.Label
}

func plainMeaning(f *Info) string {
	switch f.State {
	case "needs-you":
		return "Claude asked you something here and is waiting for your answer."
	case "ready", "done":
		return "This was settled."
	case "waiting":
		return "Some points under it are still open."
	}
	return "Still being discussed."
}

// ---------------------------------------------------------------- package list

type pkgInfo struct {
	rel, name                      string
	subjects, waiting, settled, of int
	updated                        time.Time
	current                        bool // Claude is working in it now
}

// packageRows describes every package of this repo that has a graph.
func (s *seer) packageRows() []pkgInfo {
	var rows []pkgInfo
	for _, rel := range s.pkgs {
		t := pkgTree(s.root, rel)
		g := load(t)
		p := pkgInfo{rel: rel, name: pkgName(s.root, rel), current: rel == s.st["package"]}
		for _, i := range g.IDs {
			switch {
			case g.Info[i].Kind == "head":
				p.subjects++
			default:
				p.of++
				switch g.Info[i].State {
				case "needs-you":
					p.waiting++
				case "done", "dropped", "ready":
					p.settled++
				}
			}
		}
		if info, err := os.Stat(filepath.Join(t, "GRAPH.md")); err == nil {
			p.updated = info.ModTime()
		}
		rows = append(rows, p)
	}
	return rows
}

func (s *seer) openPicker() {
	s.pkgs = repoPackages(s.root)
	s.pickRows = s.packageRows()
	s.pickSel = 0
	for i, p := range s.pickRows {
		if p.rel == s.pkg {
			s.pickSel = i
		}
	}
	s.pickOn = true
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// pickerLines draws the package list, pw wide and h tall.
func (s *seer) pickerLines(pw, h int) []string {
	if h <= 0 {
		return nil
	}
	pad := func(color, text string) string {
		text = cut(text, pw)
		return color + text + strings.Repeat(" ", max(0, pw-visible(text))) + cReset
	}
	var lines []string
	var selTop int
	lines = append(lines, pad(cBold, " Code folders"), pad(cDim, " ↑↓ pick · enter open · esc close"), pad("", ""))
	for i, p := range s.pickRows {
		if i == s.pickSel {
			selTop = len(lines)
		}
		title := " " + p.name
		if p.rel == s.pkg {
			title = "▸" + p.name
		}
		color := cBold
		if i == s.pickSel {
			color = cRev + cBold
		}
		lines = append(lines, pad(color, title))
		status := fmt.Sprintf("   %d of %d settled", p.settled, p.of)
		if p.waiting > 0 {
			lines = append(lines, pad(cOrange, fmt.Sprintf("   %d waiting for you", p.waiting)))
		}
		lines = append(lines, pad("", status))
		extra := fmt.Sprintf("   %d subject(s) · %s", p.subjects, ago(p.updated))
		lines = append(lines, pad(cDim, extra))
		if p.current {
			lines = append(lines, pad(cBlue, "   ● Claude is working here"))
		}
		lines = append(lines, pad("", ""))
	}
	if len(s.pickRows) == 0 {
		lines = append(lines, pad(cDim, " No folders yet."))
	}
	// keep the highlighted package in view
	start := 0
	if selTop+5 > h {
		start = selTop + 5 - h
	}
	lines = lines[min(start, len(lines)):]
	for len(lines) < h {
		lines = append(lines, pad("", ""))
	}
	return lines[:h]
}

// toggleWaiting lets the human overrule the model: a box that is not really
// waiting on them becomes settled, and pressing again puts it back.
const settledNote = "you marked it settled: it was not waiting on you (it said: "

func (s *seer) toggleWaiting() {
	tree := s.tree
	if abs, err := filepath.Abs(tree); err == nil {
		tree = abs // the lock is keyed by the absolute path, as the CLI takes it
	}
	release := lockTree(tree) // a worker or the CLI may be changing the same tree
	defer release()
	g := load(s.tree)
	n := g.Nodes[s.sel]
	if n == nil || g.Info[s.sel].Kind == "head" {
		return
	}
	switch n.Status {
	case "needs-you":
		appendLog(n, settledNote+oneLine(n.Outcome)+")", "human")
		n.Status, n.Outcome = "ready", "You marked this settled."
		s.msg = "marked settled"
	case "done", "ready":
		n.Status = "needs-you"
		if n.Outcome == "You marked this settled." && strings.Contains(n.Body, settledNote) {
			q := lastLogText(n, settledNote)
			n.Outcome = strings.TrimSuffix(q, ")") // undo: the question it was waiting on comes back
		}
		appendLog(n, "you marked it as waiting for you again", "human")
		s.msg = "marked waiting for you"
	default:
		s.msg = "x only flips between waiting for you and settled"
		return
	}
	changed := []*Node{n}
	settle(g, func(m *Node) {
		if m != n {
			changed = append(changed, m)
		}
	})
	g.mutate(changed...)
	s.reload()
}

// paste puts pasted text into the open prompt; with no prompt open it is
// ignored, never run as keys.
func (s *seer) paste(text string) {
	if !s.inputOn {
		s.msg = "paste ignored: press t first to type a message"
		return
	}
	s.input = append(s.input, []rune(pasteText(text))...)
}

// branch adds a new idea under a card, as Prime's "+": the card appears in the
// tree at once, and the idea goes to Claude tagged with it, so the turn that
// follows lands on the new card instead of being guessed.
func (s *seer) branch(parent, text string) {
	if icBin() == "" {
		s.msg = "needs intercom: " + icInstall
		return
	}
	if s.ended {
		s.msg = "the Claude in this tab has ended: start it again to branch off a card"
		return
	}
	tree := s.tree
	if abs, err := filepath.Abs(tree); err == nil {
		tree = abs
	}
	release := lockTree(tree)
	g := load(s.tree)
	pn := g.Nodes[parent]
	if pn == nil {
		release()
		s.msg = "that card is gone"
		return
	}
	n := newNode(g, clip(oneLine(text), 48, false), s.st["agent"], []string{parent})
	appendLog(n, "you branched this off “"+oneLine(pn.Title)+"”: "+oneLine(text), "human")
	changed := []*Node{n}
	if g.Info[parent] != nil && g.Info[parent].Kind != "head" && (pn.Status == "done" || pn.Status == "ready") {
		pn.Status = "open" // a new idea under a settled point opens it again
		appendLog(pn, "reopened: you branched `"+n.ID+"` off it", "human")
		changed = append(changed, pn)
	}
	g.mutate(changed...)
	release()
	s.reload()
	s.sel = n.ID
	pkg := pkgName(s.root, s.pkg)
	s.send(fmt.Sprintf("[code-tree · %s · %s · %s]\nNew idea, branching off “%s”: %s", pkg, n.ID, n.Title, pn.Title, text))
}
