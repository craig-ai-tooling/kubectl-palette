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

// UnsetKubeconfig is the line `use --unset` prints for the hook to eval.
const UnsetKubeconfig = "unset KUBECONFIG"

// hook wraps kubectl so `kubectl palette use` can change this shell's KUBECONFIG, which a
// child process cannot do. Everything else goes to the real kubectl untouched, or through the
// user's own kubectl alias (kubecolor, extra flags) when one was set before the hook loaded.
//
// The `function name {` form matters: zsh expands an alias in `name() {`, so with
// `alias kubectl=kubecolor` the old form defined the wrong function or failed to parse.
// The alias is removed because an alias is looked up before a function and would hide the
// hook; its text is kept in _kp_kubectl_alias and used for every other kubectl command.
const body = `function kubectl {
  if [ "$1" = "palette" ] && [ "$2" = "use" ]; then
    local _kp_out
    _kp_out="$(command kubectl-palette use "${@:3}" --print-env)" || return $?
    eval "$_kp_out"
  elif [ -n "${_kp_kubectl_alias-}" ]; then
    (unset -f kubectl; eval "$_kp_kubectl_alias \"\$@\"")
  else
    command kubectl "$@"
  fi
}
`

// keepAlias reads the alias into _kp_kubectl_alias and removes it. The alias runs in a
// subshell with this function removed, so any form that ends up at kubectl
// (alias kubectl='kubectl --context x', 'AWS_PROFILE=p kubectl', '\kubectl') reaches the
// real binary instead of calling the hook again. Loading the hook twice keeps the first alias.
const keepAlias = `if [ -n "$_kp_alias_now" ]; then
  _kp_kubectl_alias="$_kp_alias_now"
  unalias kubectl
fi
unset _kp_alias_now
`

// Init returns the hook for zsh or bash.
func Init(sh string) (string, error) {
	switch sh {
	case "zsh":
		return "_kp_alias_now=\"${aliases[kubectl]-}\"\n" + keepAlias + body, nil
	case "bash":
		return "_kp_alias_now=\"${BASH_ALIASES[kubectl]-}\"\n" + keepAlias + body, nil
	}
	return "", fmt.Errorf("unsupported shell %q: use zsh or bash", sh)
}
