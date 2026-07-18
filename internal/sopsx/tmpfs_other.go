//go:build !linux

package sopsx

import "fmt"

// DefaultTmpfsRoot is unused on non-Linux platforms.
const DefaultTmpfsRoot = "/dev/shm"

// CheckTmpfs refuses tmpfs mode outside Linux (no guaranteed tmpfs mount);
// exec-env mode remains available everywhere.
func CheckTmpfs(root string) error {
	return fmt.Errorf("sops_mode = \"tmpfs\" is only supported on linux (no tmpfs at %s)", root)
}
