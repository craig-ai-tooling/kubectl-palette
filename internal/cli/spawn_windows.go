//go:build windows

package cli

import "os/exec"

// SpawnBackgroundSync starts `<self> sync --background` without waiting for it.
func SpawnBackgroundSync(self string) error {
	cmd := exec.Command(self, "sync", "--background")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
