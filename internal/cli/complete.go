package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/craig-ai-tooling/kubectl-palette/internal/complete"
	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
)

// CompleterName is the executable kubectl runs to complete plugin arguments.
const CompleterName = "kubectl_complete-palette"

// IsCompleterName reports whether argv0 names the kubectl completer executable.
func IsCompleterName(argv0 string) bool {
	base := filepath.Base(argv0)
	base = strings.TrimSuffix(base, ".exe")
	return base == CompleterName
}

// complete prints candidates for kubectl's plugin completion protocol: one per line,
// then ":<directive>". It reads only the index file. Errors are swallowed on purpose:
// a completion failure must never print into the user's command line.
func (a *App) complete(words []string) {
	root := a.Paths.Root()
	// The cluster names come from the entry the command line selects: its --tenant, else
	// the usual selection. Any failure leaves the index empty.
	flagVal, hostVal := "", ""
	for i, w := range words[:max(len(words)-1, 0)] {
		if v, ok := strings.CutPrefix(w, "--tenant="); ok {
			flagVal = v
		} else if w == "--tenant" && i+1 < len(words)-1 {
			flagVal = words[i+1]
		}
		if v, ok := strings.CutPrefix(w, "--host="); ok {
			hostVal = v
		} else if w == "--host" && i+1 < len(words)-1 {
			hostVal = words[i+1]
		}
	}
	var idx *index.Index
	if name, err := a.pickEntry(flagVal); err == nil {
		if i, err := index.Load(root.ForTenant(name).IndexFile()); err == nil {
			idx = i
		}
	}
	// After --host <cluster>, only the virtual clusters that cluster hosts are offered.
	if idx != nil && hostVal != "" {
		if sub, err := idx.HostedBy(hostVal); err == nil {
			idx = sub
		}
	}
	var ctxNames []string
	if len(words) > 2 && words[0] == "contexts" && words[1] == "remove" && a.Paths.ContextFile() != "" {
		if recs, err := kubectx.ListFor(a.Paths.ContextFile(), a.Paths.ContextRegistry()); err == nil {
			for _, r := range recs {
				ctxNames = append(ctxNames, r.Context)
			}
		}
	}
	out := complete.CompleteAll(idx, root.Entries(), ctxNames, words)
	var b strings.Builder
	for _, s := range out {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, ":%d\n", complete.DirectiveNoFileComp)
	_, _ = io.WriteString(a.Stdout, b.String())
}

func (a *App) lookPath(name string) (string, error) {
	if a.LookPath != nil {
		return a.LookPath(name)
	}
	return exec.LookPath(name)
}

func (a *App) completerInstalled() bool {
	_, err := a.lookPath(CompleterName)
	return err == nil
}

// completion handles `completion install [--dir <dir>]`.
func (a *App) completion(args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return errors.New("usage: kubectl palette completion install [--dir <dir>]")
	}
	fs := newFlags("completion install", a.Stderr)
	dir := fs.String("dir", "", "directory on PATH to put the completer in (default: next to this binary)")
	if _, err := parseFlags(fs, args[1:]); err != nil {
		return exitError(2)
	}
	link, target, err := a.completerLink(*dir)
	if err != nil {
		return err
	}
	if err := installLink(link, target); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "installed %s -> %s\n", link, target)
	if _, err := a.lookPath(CompleterName); err != nil {
		fmt.Fprintf(a.Stderr, "%s is not on PATH, so kubectl will not find the completer. Add it to PATH.\n", filepath.Dir(link))
	} else {
		fmt.Fprintln(a.Stderr, "done: kubectl palette use <TAB> now completes cluster names (kubectl 1.26 or newer)")
	}
	return nil
}

// completerLink picks where the completer goes and what it points at. Next to the
// started-as path it points at that sibling by name, so a krew bin link keeps working
// after an upgrade. Under --dir it points at the resolved binary.
func (a *App) completerLink(dir string) (link, target string, err error) {
	name := CompleterName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", "", err
		}
		if a.RealExe == "" {
			return "", "", errors.New("cannot find this binary's path")
		}
		return filepath.Join(abs, name), a.RealExe, nil
	}
	exe := a.Exe
	if exe == "" {
		return "", "", errors.New("cannot find this binary's path: pass --dir")
	}
	if !strings.ContainsRune(exe, filepath.Separator) {
		if p, err := a.lookPath(exe); err == nil {
			exe = p
		}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	return filepath.Join(filepath.Dir(exe), name), filepath.Base(exe), nil
}

// installLink makes link point at target. An existing symlink is replaced; a regular
// file is left alone, because it may be something else. Windows gets a copy, since
// creating symlinks there needs privileges.
func installLink(link, target string) error {
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 && runtime.GOOS != "windows" {
			return fmt.Errorf("%s exists and is not a symlink: remove it first", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		src := target
		if !filepath.IsAbs(src) {
			src = filepath.Join(filepath.Dir(link), src)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(link, b, 0o755)
	}
	return os.Symlink(target, link)
}
