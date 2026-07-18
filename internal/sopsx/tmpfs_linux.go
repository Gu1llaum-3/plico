//go:build linux

package sopsx

import (
	"fmt"
	"syscall"
)

const tmpfsMagic = 0x01021994 // TMPFS_MAGIC

// DefaultTmpfsRoot is where tmpfs-mode secrets live on Linux.
const DefaultTmpfsRoot = "/dev/shm"

// CheckTmpfs verifies that root really is a tmpfs so cleartext secrets can
// never land on disk by misconfiguration.
func CheckTmpfs(root string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", root, err)
	}
	if st.Type != tmpfsMagic {
		return fmt.Errorf("%s is not a tmpfs; refusing sops_mode = \"tmpfs\"", root)
	}
	return nil
}
