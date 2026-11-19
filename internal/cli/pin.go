package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
)

// A terminal pin is a kubeconfig that holds only a current-context. `use` puts it first in
// $KUBECONFIG, ahead of the file with the recorded contexts:
//
//	KUBECONFIG=~/.kube/palette/.terminals/<id>.yaml:<context file>
//
// kubectl takes current-context from the first file that sets it, so this terminal gets its
// own current context while `kubectl config get-contexts` lists every context. kubectl writes
// a current-context change to the first file in the list, so `kubectl config use-context` in
// this terminal changes only this terminal. Each terminal keeps one pin: a later `use` finds
// it at the head of $KUBECONFIG and rewrites it.
//
// Pins are never deleted automatically. kubectl skips a missing file in $KUBECONFIG, so
// deleting the pin of an open terminal would silently move it to the global current context.

var pinName = regexp.MustCompile(`^[0-9a-f]{16}\.yaml$`)

func (a *App) terminalDir() string {
	return filepath.Join(a.Paths.Root().KubeDir, config.TerminalDir)
}

func absOr(p string) string {
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}

// terminalPin returns the pin at the head of a $KUBECONFIG value, "" when there is none.
func (a *App) terminalPin(kc string) string {
	if kc == "" {
		return ""
	}
	first := absOr(filepath.SplitList(kc)[0])
	if filepath.Dir(first) != absOr(a.terminalDir()) || !pinName.MatchString(filepath.Base(first)) {
		return ""
	}
	return first
}

// pinTerminal points this terminal at context name and returns the $KUBECONFIG value to
// export: the pin, the context file, then any other file the user had listed.
func (a *App) pinTerminal(name string) (string, error) {
	kc := a.Getenv("KUBECONFIG")
	pin := a.terminalPin(kc)
	if pin == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		pin = filepath.Join(absOr(a.terminalDir()), hex.EncodeToString(b)+".yaml")
	}
	body := fmt.Sprintf("# kubectl-palette: the current context of one terminal. See: kubectl palette current\n"+
		"apiVersion: v1\nkind: Config\ncurrent-context: %q\n", name)
	if err := config.WriteFileAtomic(pin, []byte(body)); err != nil {
		return "", err
	}
	file := absOr(a.Paths.ContextFile())
	list := []string{pin, file}
	seen := map[string]bool{pin: true, file: true}
	kube := absOr(a.Paths.Root().KubeDir)
	for _, p := range filepath.SplitList(kc) {
		if p == "" {
			continue
		}
		p = absOr(p)
		// Leave out files the plugin owns: other pins and cluster files of an earlier `use`.
		if rel, err := filepath.Rel(kube, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	return strings.Join(list, string(os.PathListSeparator)), nil
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
				if r.Context != name {
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
