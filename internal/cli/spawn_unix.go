//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// SpawnBackgroundSync starts `<self> sync --background` detached from this process.
func SpawnBackgroundSync(self string) error {
	cmd := exec.Command(self, "sync", "--background")
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
