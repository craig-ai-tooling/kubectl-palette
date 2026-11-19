package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/picker"
)

func TestCompleteIsLocalAndEndsWithDirective(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("sync")
	before := h.f.total()
	code, out, _ := h.run("__complete", "use", "")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"Default/alpha", "Default/beta", "Other/plain", ":4"}
	if strings.Join(lines, ",") != strings.Join([]string{"alpha", "beta", "plain", ":4"}, ",") {
		t.Fatalf("got %v want names then :4 (%v)", lines, want)
	}
	if h.f.total() != before {
		t.Fatal("completion made a network call")
	}
	_, out, _ = h.run("__complete", "exec", "al")
	if out != "alpha\n:4\n" {
		t.Fatalf("%q", out)
	}
	_, out, _ = h.run("__complete", "use", "--a")
	if out != "--admin\n:4\n" {
		t.Fatalf("%q", out)
	}
	_, out, _ = h.run("__complete", "u")
	if out != "use\n:4\n" {
		t.Fatalf("%q", out)
	}
}

func TestCompleteWithoutIndexStaysQuiet(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	code, out, e := h.run("__complete", "use", "")
	if code != 0 || out != ":4\n" || e != "" {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if h.f.total() != 0 {
		t.Fatal("completion must not build the index")
	}
}

func TestIsCompleterName(t *testing.T) {
	for in, want := range map[string]bool{
		"kubectl_complete-palette": true, "/usr/bin/kubectl_complete-palette": true,
		`kubectl_complete-palette.exe`: true, "kubectl-palette": false, "": false,
	} {
		if IsCompleterName(in) != want {
			t.Errorf("%q: want %v", in, want)
		}
	}
}

func TestCompletionInstallNextToBinary(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl-palette")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.a.Exe, h.a.RealExe = bin, bin
	h.a.LookPath = func(string) (string, error) { return filepath.Join(dir, CompleterName), nil }
	if code, _, e := h.run("completion", "install"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	got, err := os.Readlink(filepath.Join(dir, CompleterName))
	if err != nil || got != "kubectl-palette" {
		t.Fatalf("link %q %v: want a relative link to the sibling", got, err)
	}
	// Running it twice replaces the link and succeeds.
	if code, _, e := h.run("completion", "install"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	// A regular file in the way is not overwritten.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, CompleterName), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, e := h.run("completion", "install", "--dir", other); code == 0 || !strings.Contains(e, "not a symlink") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestCompletionInstallDirPointsAtRealBinary(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	real := filepath.Join(t.TempDir(), "kubectl-palette")
	target := t.TempDir()
	h.a.Exe, h.a.RealExe = real, real
	h.a.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	code, _, e := h.run("completion", "install", "--dir", target)
	if code != 0 || !strings.Contains(e, "not on PATH") {
		t.Fatalf("%d %s", code, e)
	}
	if got, _ := os.Readlink(filepath.Join(target, CompleterName)); got != real {
		t.Fatalf("link %q", got)
	}
}

func TestShellInitHintOnlyWhenCompleterMissing(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.LookPath = func(string) (string, error) { return "", errors.New("missing") }
	_, out, e := h.run("shell-init", "bash")
	if !strings.Contains(e, "completion install") || strings.Contains(out, "completion install") {
		t.Fatalf("hint belongs on stderr only: out=%q err=%q", out, e)
	}
	h.a.LookPath = func(string) (string, error) { return "/x/" + CompleterName, nil }
	if _, _, e = h.run("shell-init", "bash"); e != "" {
		t.Fatalf("no hint expected: %q", e)
	}
}

func TestUseFuzzyPickerAndFallbacks(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.noOIDC["ub"] = true
	h.run("sync")
	h.run("use", "beta", "--print-env") // learns that beta has no OIDC
	h.a.StdinIsTTY = true

	var items []picker.Item
	h.a.Pick = func(in []picker.Item) (int, error) { items = in; return 2, nil }
	code, out, e := h.run("use", "--print-env")
	if code != 0 || h.target(t, out) != "plain" {
		t.Fatalf("%d %q %s", code, out, e)
	}
	joined := items[0].Label + "|" + items[1].Label + "|" + items[2].Label
	if joined != "Default/alpha  admin|Default/beta  no-oidc|Other/plain  admin" {
		t.Fatalf("labels: %s", joined)
	}
	if !strings.Contains(items[1].Detail, "oidc: none") {
		t.Fatalf("detail: %s", items[1].Detail)
	}

	h.a.Pick = func([]picker.Item) (int, error) { return -1, picker.ErrAborted }
	if code, _, e = h.run("use", "--print-env"); code == 0 || !strings.Contains(e, "no cluster chosen") {
		t.Fatalf("%d %s", code, e)
	}

	// A finder that cannot start falls back to the numbered list.
	h.a.Pick = func([]picker.Item) (int, error) { return -1, errors.New("no tty") }
	h.a.Stdin = strings.NewReader("1\n")
	code, out, e = h.run("use", "--print-env")
	if code != 0 || h.target(t, out) != "alpha" || !strings.Contains(e, "numbered list") {
		t.Fatalf("%d %q %s", code, out, e)
	}

	// --no-fuzzy never calls the finder.
	h.a.Pick = func([]picker.Item) (int, error) { t.Fatal("finder must not run"); return 0, nil }
	h.a.Stdin = strings.NewReader("3\n")
	if code, out, _ = h.run("use", "--print-env", "--no-fuzzy"); code != 0 || h.target(t, out) != "plain" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestNoOIDCLearnedThenCleared(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.noOIDC["ub"] = true
	h.run("sync")
	h.run("use", "beta", "--print-env")
	h.run("sync") // a rebuild must keep what was learned
	h.a.StdinIsTTY = true
	var items []picker.Item
	h.a.Pick = func(in []picker.Item) (int, error) { items = in; return 0, nil }
	h.run("use", "--print-env")
	if !strings.Contains(items[1].Label, "no-oidc") {
		t.Fatalf("flag lost across a rebuild: %s", items[1].Label)
	}
	h.f.noOIDC["ub"] = false
	if code, _, e := h.run("use", "beta", "--print-env"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	h.run("use", "--print-env")
	if strings.Contains(items[1].Label, "no-oidc") {
		t.Fatalf("flag should clear once OIDC works: %s", items[1].Label)
	}
}
