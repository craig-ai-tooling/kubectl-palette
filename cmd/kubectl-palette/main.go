// Command kubectl-palette is a kubectl plugin for Spectro Cloud Palette.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is overridden at build time with -ldflags.
var version = "dev"

var planned = []string{"login", "use", "exec", "current", "sync", "logout", "token", "shell-init"}

const usage = `usage: kubectl palette <command>

commands:
  login        log in through the browser (not implemented yet)
  use          target a cluster in this terminal (not implemented yet)
  exec         run kubectl against a cluster once (not implemented yet)
  current      show the current target (not implemented yet)
  sync         refresh the local cluster index (not implemented yet)
  logout       remove cached credentials (not implemented yet)
  token        exec-credential plugin for kubectl (not implemented yet)
  shell-init   print the shell hook for zsh or bash (not implemented yet)
  version      print the version
`

// run executes the CLI and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd := args[0]
	if cmd == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	for _, p := range planned {
		if p == cmd {
			fmt.Fprintf(stderr, "kubectl-palette %s: not implemented yet\n", cmd)
			return 2
		}
	}
	fmt.Fprintf(stderr, "kubectl-palette: unknown command %q\n\n%s", cmd, usage)
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
