package complete

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/indextest"
)

var sizes = []int{1000, 5000}

func savedIndex(tb testing.TB, n int) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "index.json")
	if err := indextest.Synthetic(n, 50).Save(path); err != nil {
		tb.Fatal(err)
	}
	return path
}

// TestScaleBudget checks the speed budget in-process: loading the index file and
// answering a completion must stay far under 50 ms at 1,000 clusters. The bound is loose
// so a slow CI runner does not flake it; the benchmarks give the real numbers.
func TestScaleBudget(t *testing.T) {
	path := savedIndex(t, 1000)
	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		idx, err := index.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		got := Complete(idx, []string{"use", "cluster-005"})
		if len(got) == 0 {
			t.Fatal("no candidates")
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	if best > 50*time.Millisecond {
		t.Fatalf("load + complete took %v at 1000 clusters, budget 50 ms", best)
	}
}

func TestSyntheticHasCollisions(t *testing.T) {
	idx := indextest.Synthetic(1000, 50)
	count := map[string]int{}
	for _, c := range idx.Clusters {
		count[c.Name]++
	}
	dups := 0
	for _, n := range count {
		if n > 1 {
			dups++
		}
	}
	if dups < 50 {
		t.Fatalf("only %d colliding names", dups)
	}
	if _, err := idx.Resolve("cluster-00049"); err == nil {
		t.Fatal("a colliding name must be ambiguous")
	}
}

func BenchmarkLoad(b *testing.B) {
	for _, n := range sizes {
		path := savedIndex(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := index.Load(path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkComplete(b *testing.B) {
	for _, n := range sizes {
		idx := indextest.Synthetic(n, 50)
		for _, prefix := range []string{"", "cluster-00"} {
			b.Run(fmt.Sprintf("%d/prefix=%q", n, prefix), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					Complete(idx, []string{"use", prefix})
				}
			})
		}
	}
}

func BenchmarkResolve(b *testing.B) {
	for _, n := range sizes {
		idx := indextest.Synthetic(n, 50)
		for _, q := range []string{"cluster-00501", "project-07/cluster-00507", "cluster-00049", "no-such"} {
			b.Run(fmt.Sprintf("%d/%s", n, q), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					_, _ = idx.Resolve(q)
				}
			})
		}
	}
}
