//go:build !windows

package wallet

import "os"

// hardenFileACL enforces owner-only 0600 permissions on POSIX filesystems.
func hardenFileACL(path string) error {
	return os.Chmod(path, 0o600)
}
