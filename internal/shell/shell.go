// Package shell prints the shell hook and quotes values for eval.
package shell

import (
	"fmt"
	"strings"
)

// Quote single-quotes s for a POSIX shell.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ExportKubeconfig is the line the hook evals.
func ExportKubeconfig(path string) string { return "export KUBECONFIG=" + Quote(path) }

// hook wraps kubectl so `kubectl palette use` can change this shell's KUBECONFIG, which a
// child process cannot do. Everything else goes to the real kubectl untouched.
const hook = `kubectl() {
  if [ "$1" = "palette" ] && [ "$2" = "use" ]; then
    local _kp_out
    _kp_out="$(command kubectl-palette use "${@:3}" --print-env)" || return $?
    eval "$_kp_out"
  else
    command kubectl "$@"
  fi
}
`

// Init returns the hook for zsh or bash.
func Init(sh string) (string, error) {
	switch sh {
	case "bash", "zsh":
		return hook, nil
	}
	return "", fmt.Errorf("unsupported shell %q: use zsh or bash", sh)
}
