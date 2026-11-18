//go:build windows

package cli

import "os/exec"

// SpawnBackgroundSync starts `<self> sync --background [--tenant <name>]` without waiting for it.
func SpawnBackgroundSync(self, tenant string) error {
	args := []string{"sync", "--background"}
	if tenant != "" {
		args = append(args, "--tenant", tenant)
	}
	cmd := exec.Command(self, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
