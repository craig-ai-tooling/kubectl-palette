package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
	"github.com/craig-ai-tooling/kubectl-palette/internal/shell"
)

// exportValue returns the $KUBECONFIG value of a `use --print-env` line.
func exportValue(t *testing.T, out string) string {
	t.Helper()
	line := strings.TrimSpace(out)
	prefix := "export KUBECONFIG="
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("not an export line: %q", out)
	}
	v := strings.TrimPrefix(line, prefix)
	return strings.ReplaceAll(strings.Trim(v, "'"), `'\''`, "'")
}

// targetOf names what a $KUBECONFIG value selects: the pinned context, or the cluster file's
// name without .yaml when the terminal is not pinned.
func (h *harness) targetOf(t *testing.T, kc string) string {
	t.Helper()
	if pin := h.a.terminalPin(kc); pin != "" {
		name, err := kubectx.CurrentContext(pin)
		if err != nil {
			t.Fatalf("read pin: %v", err)
		}
		return name
	}
	return strings.TrimSuffix(filepath.Base(kc), ".yaml")
}

// mustRunOut runs a command that must succeed and returns its stdout.
func (h *harness) mustRunOut(t *testing.T, args ...string) string {
	t.Helper()
	code, out, e := h.run(args...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, e)
	}
	return out
}

func (h *harness) target(t *testing.T, out string) string {
	t.Helper()
	return h.targetOf(t, exportValue(t, out))
}

func TestUsePinsTerminalToRecordedContext(t *testing.T) {
	h := ctxHarness(t)
	h.env[EnvShellPID] = "4242"
	ctxFile := ctxFileOf(t, h)
	out := h.mustRunOut(t, "use", "alpha", "--print-env")
	kc := exportValue(t, out)
	parts := filepath.SplitList(kc)
	if len(parts) != 2 || parts[1] != ctxFile {
		t.Fatalf("KUBECONFIG = %q, want <pin>:%s", kc, ctxFile)
	}
	pin := parts[0]
	if h.a.terminalPin(kc) != pin {
		t.Fatalf("%s is not recognised as a pin", pin)
	}
	b, err := os.ReadFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"token", "client-key", "certificate", "server:", "exec"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("pin holds %q:\n%s", bad, b)
		}
	}
	if fi, _ := os.Stat(pin); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pin mode %v", fi)
	}
	// The global current-context is never touched.
	if cur := h.kube(t).CurrentContext; cur != "" {
		t.Errorf("context file current-context = %q", cur)
	}

	// A second use in the same terminal rewrites the same pin.
	h.env["KUBECONFIG"] = kc
	kc2 := exportValue(t, h.mustRunOut(t, "use", "beta", "--print-env"))
	if filepath.SplitList(kc2)[0] != pin || h.targetOf(t, kc2) != "beta" {
		t.Fatalf("second use: %q", kc2)
	}
	if n, _ := os.ReadDir(filepath.Dir(pin)); len(n) != 1 {
		t.Fatalf("%d pins after two uses in one terminal", len(n))
	}

	// current reads the pin.
	h.env["KUBECONFIG"] = kc2
	if out := h.mustRunOut(t, "current"); !strings.Contains(out, "Default/beta (oidc, context beta)") {
		t.Fatalf("current: %q", out)
	}

	// A new terminal gets its own pin.
	delete(h.env, "KUBECONFIG")
	h.env[EnvShellPID] = "4343"
	kc3 := exportValue(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	if filepath.SplitList(kc3)[0] == pin {
		t.Fatal("a terminal without a pin reused another terminal's pin")
	}
}

func TestUsePinKeepsUserKubeconfigFiles(t *testing.T) {
	h := ctxHarness(t)
	ctxFile := ctxFileOf(t, h)
	mine := filepath.Join(t.TempDir(), "eks.yaml")
	old := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml") // an earlier `use` without contexts
	h.env["KUBECONFIG"] = strings.Join([]string{old, mine, ctxFile}, string(os.PathListSeparator))
	kc := exportValue(t, h.mustRunOut(t, "use", "beta", "--print-env"))
	parts := filepath.SplitList(kc)
	if len(parts) != 3 || parts[1] != ctxFile || parts[2] != mine {
		t.Fatalf("KUBECONFIG = %q, want <pin>:%s:%s", kc, ctxFile, mine)
	}
}

func TestUseWithoutContextUsesClusterFile(t *testing.T) {
	h := ctxHarness(t)
	for _, args := range [][]string{{"use", "alpha", "--no-context", "--print-env"}} {
		kc := exportValue(t, h.mustRunOut(t, args...))
		if h.a.terminalPin(kc) != "" || !strings.HasSuffix(kc, "alpha.yaml") {
			t.Fatalf("%v: %q", args, kc)
		}
	}
	h.env[config.EnvNoContexts] = "1"
	kc := exportValue(t, h.mustRunOut(t, "use", "beta", "--print-env"))
	if h.a.terminalPin(kc) != "" || !strings.HasSuffix(kc, "beta.yaml") {
		t.Fatalf("contexts off: %q", kc)
	}
}

func TestUseReaddsContextRemovedByHand(t *testing.T) {
	h := ctxHarness(t)
	h.mustRunOut(t, "use", "alpha", "--print-env")
	cfg := h.kube(t)
	delete(cfg.Contexts, "alpha")
	writeKube(t, h, cfg)
	name := h.target(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	if _, ok := h.kube(t).Contexts[name]; !ok {
		t.Fatalf("pinned to %q, which is not in the context file", name)
	}
}

func TestLogoutKeepsTerminalPins(t *testing.T) {
	h := ctxHarness(t)
	kc := exportValue(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	pin := filepath.SplitList(kc)[0]
	h.run("logout")
	if _, err := os.Stat(pin); err != nil {
		t.Fatalf("logout removed the pin: %v", err)
	}
}

func TestPinExportQuotes(t *testing.T) {
	// The export line must survive eval with the list separator inside the quotes.
	line := shell.ExportKubeconfig("/a b/p.yaml" + string(os.PathListSeparator) + "/c/it's")
	if !strings.HasPrefix(line, "export KUBECONFIG='/a b/p.yaml") {
		t.Fatal(line)
	}
}

// Review finding: tmux panes and child shells inherit $KUBECONFIG. A pin belongs to the shell
// whose PID made it, so another shell gets its own and never switches the first.
func TestInheritedPinIsNotShared(t *testing.T) {
	h := ctxHarness(t)
	h.env[EnvShellPID] = "100"
	kcA := exportValue(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	pinA := filepath.SplitList(kcA)[0]
	h.env["KUBECONFIG"] = kcA // pane B inherits pane A's environment
	h.env[EnvShellPID] = "200"
	kcB := exportValue(t, h.mustRunOut(t, "use", "beta", "--print-env"))
	if filepath.SplitList(kcB)[0] == pinA {
		t.Fatal("pane B rewrote pane A's pin")
	}
	if got, _ := kubectx.CurrentContext(pinA); got != "alpha" {
		t.Fatalf("pane A switched to %q", got)
	}
	// Without the hook's variable nothing is reused either.
	delete(h.env, EnvShellPID)
	h.env["KUBECONFIG"] = kcA
	if kc := exportValue(t, h.mustRunOut(t, "use", "beta", "--print-env")); filepath.SplitList(kc)[0] == pinA {
		t.Fatal("a shell without the hook variable reused a pin")
	}
}

// Review findings: kubectl and other tools write new entries to the first file in
// $KUBECONFIG, the pin. Such a pin may carry their credentials, and an entry named like the
// Palette context would override it. A later use leaves the file alone, starts a new pin and
// names what stays behind.
func TestPinWithOtherEntriesIsLeftAlone(t *testing.T) {
	h := ctxHarness(t)
	h.env[EnvShellPID] = "300"
	kc := exportValue(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	pin := filepath.SplitList(kc)[0]
	cfg, err := clientcmd.LoadFromFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	// Another tool adds its cluster, and one named like the Palette context "beta".
	cfg.Clusters["eks"] = &api.Cluster{Server: "https://eks.example"}
	cfg.AuthInfos["eks"] = &api.AuthInfo{Token: "x"}
	cfg.Contexts["eks"] = &api.Context{Cluster: "eks", AuthInfo: "eks"}
	cfg.Contexts["beta"] = &api.Context{Cluster: "eks", AuthInfo: "eks"}
	if err := clientcmd.WriteToFile(*cfg, pin); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(pin)
	h.env["KUBECONFIG"] = kc
	if _, _, e := h.run("use", "--unset", "--print-env"); !strings.Contains(e, "context eks") {
		t.Fatalf("--unset gave no note: %s", e)
	}
	code, out, e := h.run("use", "beta", "--print-env")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	kc2 := exportValue(t, out)
	newPin := filepath.SplitList(kc2)[0]
	if newPin == pin || strings.Contains(kc2, pin) {
		t.Fatalf("the pin with other entries was reused or kept in the list: %q", kc2)
	}
	if after, _ := os.ReadFile(pin); string(after) != string(before) {
		t.Fatal("the pin with other entries was rewritten")
	}
	if b, _ := os.ReadFile(newPin); strings.Contains(string(b), "eks") || strings.Contains(string(b), "token") {
		t.Fatalf("new pin carries other entries:\n%s", b)
	}
	if h.targetOf(t, kc2) != "beta" {
		t.Fatalf("new pin selects %q", h.targetOf(t, kc2))
	}
	if !strings.Contains(e, "holds cluster eks, context beta, context eks, user eks") || !strings.Contains(e, "no longer lists it") {
		t.Fatalf("no note: %s", e)
	}
}

// Review finding: preferences or extensions in a pin count as other content too.
func TestPinWithPreferencesIsLeftAlone(t *testing.T) {
	h := ctxHarness(t)
	h.env[EnvShellPID] = "400"
	kc := exportValue(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	pin := filepath.SplitList(kc)[0]
	cfg, err := clientcmd.LoadFromFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Preferences.Colors = true
	if err := clientcmd.WriteToFile(*cfg, pin); err != nil {
		t.Fatal(err)
	}
	h.env["KUBECONFIG"] = kc
	code, out, e := h.run("use", "beta", "--print-env")
	if code != 0 || filepath.SplitList(exportValue(t, out))[0] == pin || !strings.Contains(e, "holds preferences") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if got, _ := clientcmd.LoadFromFile(pin); got == nil || !got.Preferences.Colors {
		t.Fatal("preferences were dropped")
	}
}

// Review finding: a recorded context the user re-pointed must not be selected on a warm use.
func TestWarmUseIgnoresRepointedContext(t *testing.T) {
	h := ctxHarness(t)
	h.mustRunOut(t, "use", "alpha", "--print-env")
	cfg := h.kube(t)
	cfg.Clusters["mine"] = &api.Cluster{Server: "https://mine.example"}
	cfg.Contexts["alpha"].Cluster = "mine"
	writeKube(t, h, cfg)
	name := h.target(t, h.mustRunOut(t, "use", "alpha", "--print-env"))
	if name == "alpha" {
		t.Fatal("warm use selected the re-pointed context")
	}
	k := h.kube(t)
	if c := k.Contexts[name]; c == nil || c.Cluster != name {
		t.Fatalf("pinned to %q, which is not the plugin's context: %+v", name, c)
	}
	if k.Contexts["alpha"].Cluster != "mine" {
		t.Fatal("the user's re-pointed context was changed")
	}
}
