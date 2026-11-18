// Package picker is the incremental fuzzy finder behind `kubectl palette use` with no name.
//
// It is written against golang.org/x/term instead of a terminal UI library. The libraries
// tried (go-fuzzyfinder, which pulls in tcell) add about 26 ms to the start of every
// process, and `kubectl-palette token` runs on every kubectl call.
package picker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// ErrAborted means the user left the picker without choosing.
var ErrAborted = errors.New("no cluster chosen")

// ErrUnavailable means there is no usable terminal, so the caller should fall back.
var ErrUnavailable = errors.New("no usable terminal")

// Item is one row. Label is what the user sees and what the fuzzy match runs on, so any
// status words in it (admin, no-oidc) can be typed to filter. Detail is shown for the
// highlighted row on the last line.
type Item struct {
	Label  string
	Detail string
}

// Pick opens the finder over items on the controlling terminal and returns the chosen
// index. It works while stdout is captured by the shell hook, because it never uses stdout.
func Pick(items []Item) (int, error) {
	if runtime.GOOS == "windows" {
		return -1, fmt.Errorf("%w: the fuzzy finder is not available on Windows", ErrUnavailable)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return -1, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer tty.Close()
	fd := int(tty.Fd())
	cols, rows, err := term.GetSize(fd)
	if err != nil || rows < 4 || cols < 20 {
		return -1, fmt.Errorf("%w: terminal too small", ErrUnavailable)
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = term.Restore(fd, old) }()
	return Run(tty, tty, items, rows, cols)
}

// Run drives the finder over in and out. It is split from Pick so tests can feed keys.
func Run(in io.Reader, out io.Writer, items []Item, rows, cols int) (int, error) {
	w := bufio.NewWriter(out)
	// Alternate screen, so the user's scrollback is untouched, and restore on every exit.
	fmt.Fprint(w, "\x1b[?1049h\x1b[?25l")
	defer func() { fmt.Fprint(w, "\x1b[?25h\x1b[?1049l"); _ = w.Flush() }()

	s := &state{items: items, rows: rows, cols: cols}
	s.refilter()
	s.draw(w)
	_ = w.Flush()

	buf := make([]byte, 64)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return -1, ErrAborted
		}
		if act := s.keys(buf[:n]); act != actNone {
			if act == actAbort || len(s.shown) == 0 {
				return -1, ErrAborted
			}
			return s.shown[s.cur], nil
		}
		s.draw(w)
		_ = w.Flush()
	}
}

type action int

const (
	actNone action = iota
	actAbort
	actChoose
)

type state struct {
	items      []Item
	rows, cols int
	query      []rune
	shown      []int // indexes into items, best first
	cur, top   int
}

func (s *state) listHeight() int { return s.rows - 3 }

// keys applies one read of terminal input. A read normally holds one key or one escape
// sequence; pasted text arrives as several runes in one read.
func (s *state) keys(b []byte) action {
	for len(b) > 0 {
		switch {
		case b[0] == 0x1b:
			if len(b) == 1 {
				return actAbort
			}
			seq, rest := escape(b)
			b = rest
			switch seq {
			case "up":
				s.move(-1)
			case "down":
				s.move(1)
			case "pgup":
				s.move(-s.listHeight())
			case "pgdn":
				s.move(s.listHeight())
			}
			continue
		case b[0] == 3 || b[0] == 4: // ctrl-c, ctrl-d
			return actAbort
		case b[0] == '\r' || b[0] == '\n':
			return actChoose
		case b[0] == 0x10 || b[0] == 0x0b: // ctrl-p, ctrl-k
			s.move(-1)
		case b[0] == 0x0e: // ctrl-n
			s.move(1)
		case b[0] == 0x7f || b[0] == 0x08:
			if len(s.query) > 0 {
				s.query = s.query[:len(s.query)-1]
				s.refilter()
			}
		case b[0] == 0x15: // ctrl-u
			s.query = nil
			s.refilter()
		case b[0] == 0x17: // ctrl-w
			q := strings.TrimRight(string(s.query), " ")
			if i := strings.LastIndexByte(q, ' '); i >= 0 {
				q = q[:i+1]
			} else {
				q = ""
			}
			s.query = []rune(q)
			s.refilter()
		case b[0] < 0x20:
			// other control keys do nothing
		default:
			r, size := utf8.DecodeRune(b)
			if r != utf8.RuneError && unicode.IsPrint(r) {
				s.query = append(s.query, r)
				s.refilter()
			}
			b = b[size:]
			continue
		}
		b = b[1:]
	}
	return actNone
}

// escape decodes the arrow and page keys from a sequence starting with ESC.
func escape(b []byte) (string, []byte) {
	if len(b) >= 3 && (b[1] == '[' || b[1] == 'O') {
		switch b[2] {
		case 'A':
			return "up", b[3:]
		case 'B':
			return "down", b[3:]
		}
		if len(b) >= 4 && b[3] == '~' {
			switch b[2] {
			case '5':
				return "pgup", b[4:]
			case '6':
				return "pgdn", b[4:]
			}
			return "", b[4:]
		}
		return "", b[3:]
	}
	return "", b[1:]
}

func (s *state) move(d int) {
	if len(s.shown) == 0 {
		return
	}
	s.cur += d
	if s.cur < 0 {
		s.cur = 0
	}
	if s.cur >= len(s.shown) {
		s.cur = len(s.shown) - 1
	}
	s.scroll()
}

func (s *state) scroll() {
	h := s.listHeight()
	if s.cur < s.top {
		s.top = s.cur
	}
	if s.cur >= s.top+h {
		s.top = s.cur - h + 1
	}
}

func (s *state) refilter() {
	s.shown = Filter(s.items, string(s.query))
	s.cur, s.top = 0, 0
}

func (s *state) draw(w io.Writer) {
	fmt.Fprint(w, "\x1b[H")
	fmt.Fprintf(w, "> %s\x1b[K\r\n", clip(string(s.query), s.cols-2))
	fmt.Fprintf(w, "\x1b[2m  %d/%d  (up/down, enter to choose, esc to cancel)\x1b[0m\x1b[K\r\n", len(s.shown), len(s.items))
	h := s.listHeight()
	for row := 0; row < h; row++ {
		i := s.top + row
		if i < len(s.shown) {
			label := clip(s.items[s.shown[i]].Label, s.cols-2)
			if i == s.cur {
				fmt.Fprintf(w, "\x1b[7m> %s\x1b[0m\x1b[K\r\n", label)
			} else {
				fmt.Fprintf(w, "  %s\x1b[K\r\n", label)
			}
		} else {
			fmt.Fprint(w, "\x1b[K\r\n")
		}
	}
	detail := ""
	if len(s.shown) > 0 {
		detail = strings.ReplaceAll(s.items[s.shown[s.cur]].Detail, "\n", "  |  ")
	}
	fmt.Fprintf(w, "\x1b[2m%s\x1b[0m\x1b[K", clip(detail, s.cols))
}

func clip(s string, n int) string {
	if n < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Filter returns the indexes of items matching query, best first. The query is split on
// spaces and every term must match. A term matches as a substring or, failing that, as a
// subsequence. Case is ignored. An empty query keeps the original order.
func Filter(items []Item, query string) []int {
	terms := strings.Fields(strings.ToLower(query))
	type hit struct{ idx, score int }
	hits := make([]hit, 0, len(items))
	for i, it := range items {
		if len(terms) == 0 {
			hits = append(hits, hit{i, 0})
			continue
		}
		label := strings.ToLower(it.Label)
		total, ok := 0, true
		for _, t := range terms {
			sc, m := termScore(label, t)
			if !m {
				ok = false
				break
			}
			total += sc
		}
		if ok {
			hits = append(hits, hit{i, total})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.idx
	}
	return out
}

func boundary(label string, i int) bool {
	if i == 0 {
		return true
	}
	switch label[i-1] {
	case '/', '-', '_', ' ', '.':
		return true
	}
	return false
}

// termScore scores one lower-case term against a lower-case label.
func termScore(label, t string) (int, bool) {
	if i := strings.Index(label, t); i >= 0 {
		sc := 1000 - i
		if boundary(label, i) {
			sc += 200
		}
		return sc, true
	}
	// Subsequence: every rune in order, scored by how tight and how boundary-aligned.
	sc, last, li := 0, -2, 0
	for _, r := range t {
		found := false
		for li < len(label) {
			lr, size := utf8.DecodeRuneInString(label[li:])
			pos := li
			li += size
			if lr == r {
				sc += 10
				if pos == last+1 {
					sc += 15
				}
				if boundary(label, pos) {
					sc += 20
				}
				last, found = pos, true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return sc - len(label)/4, true
}
