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
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	boxW   = 28 // outer width of a node box
	boxH   = 5  // outer height
	slotW  = boxW + 3
	layerH = boxH + 2
)

type cell struct {
	r     rune
	color string
}

type seer struct {
	session  string // Claude session id being followed
	st       convoState
	root     string
	pkgs     []string
	pkg      string // package shown
	pinned   bool   // the human picked a package; stop following
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
	lastSeen string
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
	s := &seer{session: id, stty: strings.TrimSpace(string(saved)), results: make(chan string, 8)}
	stty("-icanon", "-echo", "min", "1")
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer func() {
		if s.serve != nil && s.serve.Process != nil {
			_ = s.serve.Process.Kill()
		}
		fmt.Print("\x1b[?25h\x1b[?1049l")
		stty(s.stty)
	}()

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

	s.size()
	s.reload()
	s.render()
	for {
		select {
		case k, ok := <-keys:
			if !ok {
				return
			}
			for _, one := range splitKeys(k) {
				if !s.key(one) {
					return
				}
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
		best, seen := "", ""
		icEntries, _ := os.ReadDir(filepath.Join(icHome(), "sessions"))
		for _, e := range icEntries {
			kv := icSession(e.Name())
			if kv["tab"] == tab && kv["kind"] == "claude" && kv["ended"] == "" && kv["seen"] > seen {
				best, seen = kv["id"], kv["seen"]
			}
		}
		if best != "" {
			return best
		}
	}
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

func (s *seer) reload() {
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
	if s.g.Nodes[s.head] == nil && len(heads) > 0 {
		s.head = heads[len(heads)-1]
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
		s.drawEdges(id)
	}
	for id := range s.pos {
		s.drawBox(id)
	}
}

func (s *seer) put(x, y int, r rune, color string) {
	if y >= 0 && y < len(s.canvas) && x >= 0 && x < len(s.canvas[y]) {
		s.canvas[y][x] = cell{r, color}
	}
}

func (s *seer) text(x, y int, str, color string) {
	for i, r := range []rune(str) {
		s.put(x+i, y, r, color)
	}
}

func hot(state string) bool { return state == "needs-you" || state == "ready" }

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
	pc, y := p[0]+boxW/2, p[1]+boxH
	minc, maxc := pc, pc
	for _, c := range kids {
		cc := s.pos[c][0] + boxW/2
		minc, maxc = min(minc, cc), max(maxc, cc)
	}
	edge := cDim
	for _, c := range kids {
		if hot(s.g.Info[c].State) {
			edge = cOrange
		}
	}
	s.put(pc, y, '│', edge)
	for x := minc; x <= maxc; x++ {
		s.put(x, y+1, '─', edge)
	}
	for _, c := range kids {
		cc := s.pos[c][0] + boxW/2
		col := cDim
		if hot(s.g.Info[c].State) {
			col = cOrange
		}
		switch {
		case minc == maxc:
			s.put(cc, y+1, '│', col)
		case cc == minc:
			s.put(cc, y+1, '╭', col)
		case cc == maxc:
			s.put(cc, y+1, '╮', col)
		default:
			s.put(cc, y+1, '┬', col)
		}
	}
	if minc != maxc {
		j := '┴'
		if pc == minc || pc == maxc {
			j = '┼'
			if pc == minc {
				j = '├'
			} else {
				j = '┤'
			}
		}
		for _, c := range kids {
			if s.pos[c][0]+boxW/2 == pc {
				j = '┼'
			}
		}
		s.put(pc, y+1, j, edge)
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

func (s *seer) drawBox(id string) {
	n, f := s.g.Nodes[id], s.g.Info[id]
	p := s.pos[id]
	x, y := p[0], p[1]
	border := cDim
	if hot(f.State) {
		border = cOrange
	}
	if f.State == "done" || f.State == "dropped" {
		border = cDim
	}
	if id == s.sel {
		border = cBold
	}
	inner := boxW - 2
	s.put(x, y, '╭', border)
	s.put(x+boxW-1, y, '╮', border)
	s.put(x, y+boxH-1, '╰', border)
	s.put(x+boxW-1, y+boxH-1, '╯', border)
	for i := 1; i < boxW-1; i++ {
		s.put(x+i, y, '─', border)
		s.put(x+i, y+boxH-1, '─', border)
	}
	for j := 1; j < boxH-1; j++ {
		s.put(x, y+j, '│', border)
		s.put(x+boxW-1, y+j, '│', border)
		s.text(x+1, y+j, strings.Repeat(" ", inner), "")
	}
	// an edge arriving from above
	if len(f.Parents) > 0 {
		col := cDim
		if hot(f.State) {
			col = cOrange
		}
		s.put(x+boxW/2, y, '┴', col)
	}
	kind := strings.ToUpper(f.Kind)
	if id == s.st["current"] {
		kind = "▶ " + kind
	}
	label := f.Label
	if f.Kind == "thread" && f.State == "open" {
		label = "Open"
	}
	labelColor := cDim
	switch f.State {
	case "needs-you", "ready":
		labelColor = cOrange
	case "done":
		labelColor = cGreen
	}
	s.text(x+2, y+1, cut(kind, inner-visible(label)-3), cDim)
	s.text(x+boxW-2-visible(label), y+1, label, labelColor)
	titleColor := ""
	if id == s.sel {
		titleColor = cBold
	}
	for k, line := range wrapTitle(n.Title, inner-2, 2) {
		s.text(x+2, y+2+k, line, titleColor)
	}
}

func (s *seer) render() {
	var out []string
	add := func(color, plain string) { out = append(out, color+cut(plain, s.w)+cReset) }

	name := s.st["agent"]
	state := "idle"
	if s.st["busy"] != "" {
		state = "working…"
	}
	topic := ""
	if s.head != "" {
		topic = s.g.Nodes[s.head].Title + " in "
	}
	head := fmt.Sprintf(" %s%s · %d", topic, pkgName(s.root, s.pkg), len(s.pos))
	right := fmt.Sprintf("following %s (%s)", name, state)
	pad := s.w - visible(head) - visible(right) - 1
	if pad < 1 {
		pad = 1
	}
	add(cBold, head+strings.Repeat(" ", pad)+right)
	add(cDim, strings.Repeat("─", s.w))

	if s.overlay != nil {
		for _, l := range s.overlay {
			if len(out) < s.h-1 {
				add("", " "+l)
			}
		}
		s.flush(out)
		return
	}

	graphH := s.h - len(out) - 4
	if s.canvas == nil {
		add(cDim, " Nothing here yet. The tree grows as you and Claude talk about this package.")
	} else {
		// keep the selected box in view, centred when the graph is bigger than the pane
		sp := s.pos[s.sel]
		W, H := len(s.canvas[0]), len(s.canvas)
		ox, oy := 0, 0
		if W > s.w {
			ox = min(max(0, sp[0]+boxW/2-s.w/2), W-s.w)
		} else {
			ox = -(s.w - W) / 2
		}
		if H > graphH {
			oy = min(max(0, sp[1]+boxH/2-graphH/2), H-graphH)
		}
		for row := 0; row < graphH; row++ {
			y := oy + row
			var b strings.Builder
			last := ""
			for col := 0; col < s.w; col++ {
				x := ox + col
				c := cell{r: ' '}
				if y >= 0 && y < H && x >= 0 && x < W {
					c = s.canvas[y][x]
				}
				if c.color != last {
					b.WriteString(cReset + c.color)
					last = c.color
				}
				b.WriteRune(c.r)
			}
			out = append(out, b.String()+cReset)
		}
	}
	for len(out) < s.h-3 {
		out = append(out, "")
	}
	add(cDim, strings.Repeat("─", s.w))
	if n := s.g.Nodes[s.sel]; n != nil {
		add("", " "+cut(n.Title+" · "+s.g.Info[s.sel].Label+" · "+n.Outcome, s.w-2))
	} else {
		add("", "")
	}
	if s.inputOn {
		add(cOrange, " "+s.prompt+" › "+string(s.input)+"▏")
	} else {
		add(cDim, " ←↑↓→ move · t talk · s switch Claude here · enter details · o topic · p package · b browser · q   "+s.msg)
	}
	s.flush(out)
}

func (s *seer) flush(lines []string) {
	if len(lines) > s.h {
		lines = lines[:s.h]
	}
	fmt.Print("\x1b[H\x1b[2J" + strings.Join(lines, "\r\n"))
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
	case "s":
		n := s.g.Nodes[s.sel]
		s.send(fmt.Sprintf("[code-tree · %s · %s] Let's switch to the thread “%s” in %s. Where it stands: %s Pick it up from there.",
			pkgName(s.root, s.pkg), s.sel, n.Title, pkgName(s.root, s.pkg), n.Outcome))
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
		if len(s.pkgs) > 0 {
			i := sort.SearchStrings(nil, "")
			for j, p := range s.pkgs {
				if p == s.pkg {
					i = j
				}
			}
			s.pkg, s.pinned, s.head, s.sel = s.pkgs[(i+1)%len(s.pkgs)], true, "", ""
			s.reload()
		}
	case "f":
		s.pinned = false
		s.reload()
		s.msg = "following " + s.st["agent"] + " again"
	case "b":
		s.browser()
	case "?":
		s.overlay = []string{
			"code-tree · keys", "",
			"  ← → ↑ ↓     move between threads (h j k l too)",
			"  enter       what the thread is about and how it went",
			"  t           talk to Claude about this thread",
			"  s           switch Claude to this thread",
			"  o           next topic in this package",
			"  p           next package",
			"  f           follow Claude again (after o or p)",
			"  b           open this graph in the browser",
			"  q / esc     close", "",
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

// sideways moves to the nearest box on the same row.
func (s *seer) sideways(dir int) {
	cur := s.pos[s.sel]
	best, bd := "", 1<<30
	for id, p := range s.pos {
		if p[1] != cur[1] || id == s.sel {
			continue
		}
		d := (p[0] - cur[0]) * dir
		if d > 0 && d < bd {
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
		f.Label + " · " + strings.ToLower(f.Kind) + " in " + pkgName(s.root, s.pkg),
		"",
	}
	if n.Outcome != "" {
		lines = append(lines, "Where it stands: "+n.Outcome, "")
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
	s.msg = "sending…"
	go func() {
		cmd := exec.Command(bin, "send", "--topic", "code-tree", "--", s.session, text)
		cmd.Env = append(os.Environ(), "IC_ID=", "CLAUDECODE=")
		out, _ := cmd.CombinedOutput()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		s.results <- strings.TrimPrefix(lines[len(lines)-1], "ic: ")
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
	cmd := exec.Command(exe, "serve", "--port", "0")
	cmd.Env = append(os.Environ(), "CT_TREE="+s.tree)
	if err := cmd.Start(); err != nil {
		s.msg = err.Error()
		return
	}
	s.serve = cmd
	s.msg = "opened in the browser"
}
