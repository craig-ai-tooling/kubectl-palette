package picker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func labels(ls ...string) []Item {
	var out []Item
	for _, l := range ls {
		out = append(out, Item{Label: l, Detail: "d-" + l})
	}
	return out
}

func TestFilter(t *testing.T) {
	items := labels("Default/alpha  admin", "Default/beta  no-oidc", "Team-B/payments-prod", "Team-B/payments-dev  admin", "Other/plain")
	for _, tc := range []struct {
		name, q string
		first   string
		count   int
	}{
		{"empty keeps all in order", "", "Default/alpha  admin", 5},
		{"substring", "pay", "Team-B/payments-prod", 2},
		{"status word filters", "admin", "Default/alpha  admin", 2},
		{"two terms are ANDed", "pay admin", "Team-B/payments-dev  admin", 1},
		{"subsequence", "tbpp", "Team-B/payments-prod", 1},
		{"case ignored", "DEFAULT/BETA", "Default/beta  no-oidc", 1},
		{"no match", "zzz", "", 0},
		{"boundary start beats mid-word", "pl", "Other/plain", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(items, tc.q)
			if len(got) != tc.count {
				t.Fatalf("got %d hits %v", len(got), got)
			}
			if tc.count > 0 && items[got[0]].Label != tc.first {
				t.Fatalf("first %q want %q", items[got[0]].Label, tc.first)
			}
		})
	}
}

// chunks returns one scripted read at a time, like a terminal delivering keys.
type chunks [][]byte

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}

func run(t *testing.T, items []Item, keys ...string) (int, error, string) {
	t.Helper()
	var in chunks
	for _, k := range keys {
		in = append(in, []byte(k))
	}
	var out bytes.Buffer
	i, err := Run(&in, &out, items, 10, 60)
	return i, err, out.String()
}

func TestRunKeys(t *testing.T) {
	items := labels("a/one", "a/two", "b/three", "b/four")
	for _, tc := range []struct {
		name string
		keys []string
		want int
		err  error
	}{
		{"enter takes the first row", []string{"\r"}, 0, nil},
		{"down down enter", []string{"\x1b[B", "\x1b[B", "\r"}, 2, nil},
		{"down then up", []string{"\x1b[B", "\x1b[A", "\r"}, 0, nil},
		{"ctrl-n and ctrl-p", []string{"\x0e", "\x0e", "\x10", "\r"}, 1, nil},
		{"type to filter", []string{"thr", "\r"}, 2, nil},
		{"typed one key at a time", []string{"f", "o", "u", "\r"}, 3, nil},
		{"backspace widens", []string{"fou", "\x7f", "\x7f", "\x7f", "\x1b[B", "\r"}, 1, nil},
		{"ctrl-u clears", []string{"zzz", "\x15", "\r"}, 0, nil},
		{"escape alone aborts", []string{"\x1b"}, -1, ErrAborted},
		{"ctrl-c aborts", []string{"\x03"}, -1, ErrAborted},
		{"enter with no match aborts", []string{"zzz", "\r"}, -1, ErrAborted},
		{"closed input aborts", nil, -1, ErrAborted},
		{"cannot move past the end", []string{"\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B", "\r"}, 3, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err, _ := run(t, items, tc.keys...)
			if got != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("got %d %v want %d %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestRunDrawsAndRestoresTerminal(t *testing.T) {
	_, _, out := run(t, labels("a/one", "b/two"), "\r")
	for _, want := range []string{"\x1b[?1049h", "> a/one", "2/2", "d-a/one", "\x1b[?25h\x1b[?1049l"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q", want)
		}
	}
}

func TestRunScrollsLongLists(t *testing.T) {
	var items []Item
	for i := 0; i < 100; i++ {
		items = append(items, Item{Label: fmt.Sprintf("p/c-%03d", i)})
	}
	got, err, out := run(t, items, "\x1b[6~", "\x1b[6~", "\r") // two page-downs, list height 7
	if err != nil || got != 14 {
		t.Fatalf("got %d %v", got, err)
	}
	if !strings.Contains(out, "p/c-014") {
		t.Fatal("the chosen row was never drawn")
	}
}

func TestFilterSpeedAt5000(t *testing.T) {
	var items []Item
	for i := 0; i < 5000; i++ {
		items = append(items, Item{Label: fmt.Sprintf("project-%02d/cluster-%05d  admin", i%50, i)})
	}
	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		Filter(items, "p07 cl1")
		if d := time.Since(start); d < best {
			best = d
		}
	}
	if best > 100*time.Millisecond {
		t.Fatalf("one keystroke over 5000 rows took %v", best)
	}
}

func BenchmarkFilter(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		var items []Item
		for i := 0; i < n; i++ {
			items = append(items, Item{Label: fmt.Sprintf("project-%02d/cluster-%05d  admin", i%50, i)})
		}
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				Filter(items, "p07 cl1")
			}
		})
	}
}
