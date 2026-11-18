package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
)

// testPaths puts every file an App can write, the context file included, under dir.
func testPaths(dir string) config.Paths {
	return config.Paths{Home: filepath.Join(dir, "home"), KubeDir: filepath.Join(dir, "kube")}.
		WithContextFile(filepath.Join(dir, "kubeconfig"))
}

// TestMain points HOME at an empty directory for the whole package, so a code path that
// reaches for the user's kubeconfig lands in a throwaway directory, and fails the run when
// that directory ends up holding one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kp-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	if _, err := os.Stat(filepath.Join(home, ".kube")); err == nil {
		fmt.Fprintln(os.Stderr, "a test wrote under $HOME/.kube: it reached the real kubeconfig location")
		code = 1
	}
	os.RemoveAll(home)
	os.Exit(code)
}

// ctxFileOf is the context file of a harness, which must sit inside the harness's temp dir.
func ctxFileOf(t *testing.T, h *harness) string {
	t.Helper()
	f := h.a.Paths.ContextFile()
	if f == "" || filepath.Dir(f) != filepath.Dir(h.a.Paths.Home) {
		t.Fatalf("test App context file %q is not inside the test directory", f)
	}
	return f
}
