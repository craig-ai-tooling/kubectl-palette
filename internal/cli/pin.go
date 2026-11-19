package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
)

// A terminal pin is a kubeconfig that sets only a current-context. `use` puts it first in
// $KUBECONFIG, ahead of the file with the recorded contexts:
//
//	KUBECONFIG=<plugin home>/terminals/<shell pid>-<random>.yaml:<context file>
//
// kubectl takes current-context from the first file that sets it, so this terminal gets its
// own current context while `kubectl config get-contexts` lists every context. kubectl writes
// a current-context change to the first file in the list, so `kubectl config use-context` in
// this terminal changes only this terminal.
//
// kubectl and other tools (aws eks update-kubeconfig, kind) also write NEW entries to the
// first file in the list. `use` therefore rewrites the pin in place and keeps whatever else
// it holds, and says so, instead of replacing it.
//
// A pin belongs to one shell: its name starts with the PID the shell hook passes in
// KUBECTL_PALETTE_SHELL. A later `use` reuses the pin only when that PID matches, so tmux
// panes or child shells that inherit $KUBECONFIG get pins of their own instead of switching
// each other. Without the variable (no hook, or a hook from an older release) every `use`
// makes a new pin.
//
// Pins are never deleted automatically. kubectl skips a missing file in $KUBECONFIG, so
// deleting the pin of an open terminal would silently move it to the global current context.

// EnvShellPID is set by the shell hook to the PID of the interactive shell.
const EnvShellPID = "KUBECTL_PALETTE_SHELL"

// TerminalsDir is the subdirectory of the root plugin home that holds the pins.
const TerminalsDir = "terminals"

var pinName = regexp.MustCompile(`^([0-9]+)-[0-9a-f]{16}\.yaml$`)

func absOr(p string) string {
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}

func (a *App) terminalDir() string {
	return absOr(filepath.Join(a.Paths.Root().Home, TerminalsDir))
}

// terminalPin returns the pin at the head of a $KUBECONFIG value, "" when there is none.
func (a *App) terminalPin(kc string) string {
	if kc == "" {
		return ""
	}
	first := absOr(filepath.SplitList(kc)[0])
	if filepath.Dir(first) != a.terminalDir() || !pinName.MatchString(filepath.Base(first)) {
		return ""
	}
	return first
}

// ownPin returns the pin at the head of $KUBECONFIG when it belongs to this shell.
func (a *App) ownPin(kc string) string {
	pin := a.terminalPin(kc)
	shellPID := a.getenv(EnvShellPID)
	if pin == "" || shellPID == "" {
		return ""
	}
	if m := pinName.FindStringSubmatch(filepath.Base(pin)); m == nil || m[1] != shellPID {
		return ""
	}
	return pin
}

func newPinPath(dir, shellPID string) (string, error) {
	if _, err := strconv.Atoi(shellPID); err != nil || shellPID == "" {
		shellPID = "0"
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return filepath.Join(dir, shellPID+"-"+hex.EncodeToString(b)+".yaml"), nil
}

// extraNames lists what a pin holds besides its current-context.
func extraNames(cfg *api.Config) []string {
	var out []string
	for n := range cfg.Contexts {
		out = append(out, "context "+n)
	}
	for n := range cfg.Clusters {
		out = append(out, "cluster "+n)
	}
	for n := range cfg.AuthInfos {
		out = append(out, "user "+n)
	}
	sort.Strings(out)
	return out
}

// writePin sets current-context in the pin and keeps everything else it holds.
func writePin(pin, name string) (extra []string, err error) {
	cfg, err := clientcmd.LoadFromFile(pin)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = api.NewConfig()
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", pin, err)
	}
	cfg.CurrentContext = name
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, err
	}
	if err := config.WriteFileAtomic(pin, b); err != nil {
		return nil, err
	}
	return extraNames(cfg), nil
}

// pinTerminal points this terminal at context name and returns the $KUBECONFIG value to
// export: the pin, the context file, then any other file the user had listed.
func (a *App) pinTerminal(name string) (string, error) {
	kc := a.getenv("KUBECONFIG")
	pin := a.ownPin(kc)
	if pin == "" {
		p, err := newPinPath(a.terminalDir(), a.getenv(EnvShellPID))
		if err != nil {
			return "", err
		}
		pin = p
	}
	extra, err := writePin(pin, name)
	if err != nil {
		return "", err
	}
	if len(extra) > 0 {
		fmt.Fprintf(a.Stderr, "note: this terminal's file %s also holds %s, added while the terminal was pinned. Only this terminal sees them.\n",
			shortPath(pin), strings.Join(extra, ", "))
	}
	file := absOr(a.Paths.ContextFile())
	list := []string{pin, file}
	seen := map[string]bool{pin: true, file: true}
	for _, p := range filepath.SplitList(kc) {
		if p == "" {
			continue
		}
		p = absOr(p)
		// Leave out files the plugin owns: other pins and cluster files of an earlier `use`.
		if a.pluginOwned(p) {
			continue
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	return strings.Join(list, string(os.PathListSeparator)), nil
}

// pluginOwned reports whether p is a pin or a cluster file of this plugin.
func (a *App) pluginOwned(p string) bool {
	if filepath.Dir(p) == a.terminalDir() {
		return true
	}
	kube := absOr(a.Paths.Root().KubeDir)
	rel, err := filepath.Rel(kube, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// noteUnpinnedExtras warns, on `use --unset`, when the pin being left holds entries that a
// tool added while the terminal was pinned.
func (a *App) noteUnpinnedExtras() {
	pin := a.terminalPin(a.getenv("KUBECONFIG"))
	if pin == "" {
		return
	}
	cfg, err := clientcmd.LoadFromFile(pin)
	if err != nil {
		return
	}
	if extra := extraNames(cfg); len(extra) > 0 {
		fmt.Fprintf(a.Stderr, "note: %s holds %s, added while the terminal was pinned. They stay in that file.\n",
			shortPath(pin), strings.Join(extra, ", "))
	}
}

// currentPinned reports the cluster behind a pinned terminal's current context.
func (a *App) currentPinned(pin, kc string) error {
	name, err := kubectx.CurrentContext(pin)
	if err != nil || name == "" {
		fmt.Fprintln(a.Stdout, "no Palette cluster selected in this terminal")
		return exitError(1)
	}
	files := filepath.SplitList(kc)
	if len(files) > 1 {
		if recs, err := kubectx.ListFor(files[1], a.Paths.ContextRegistry()); err == nil {
			for _, r := range recs {
				if r.Context != name || !kubectx.OwnsContext(files[1], r) {
					continue
				}
				prefix := ""
				if r.Tenant != "" && r.Tenant != config.DefaultEntry {
					prefix = r.Tenant + ": "
				}
				fmt.Fprintf(a.Stdout, "%s%s/%s (oidc, context %s)\n", prefix, r.Project, r.Cluster, name)
				return nil
			}
		}
	}
	fmt.Fprintf(a.Stdout, "context %s (not a Palette cluster)\n", name)
	return exitError(1)
}
