// Command kubectl-palette is a kubectl plugin for Spectro Cloud Palette.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/craig-ai-tooling/kubectl-palette/internal/cli"
	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
)

// version is overridden at build time with -ldflags.
var version = "dev"

func main() {
	paths, err := config.DefaultPaths(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubectl-palette:", err)
		os.Exit(1)
	}
	self, _ := os.Executable()
	app := &cli.App{
		Version: version, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Paths: paths,
		StdoutIsTTY: cli.IsTTY(os.Stdout), StdinIsTTY: cli.IsTTY(os.Stdin),
		OpenBrowser: cli.OpenBrowserDefault,
		SpawnSync:   func() error { return cli.SpawnBackgroundSync(self) },
		RunKubectl:  cli.RunKubectlDefault,
		SpawnShell:  cli.SpawnShellDefault,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(app.Run(ctx, os.Args[1:]))
}
