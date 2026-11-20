//go:build !windows

// Package ownerfile keeps a file that holds a credential readable by its
// owner only: mode 0600 on Unix, a protected DACL on Windows (#206).
package ownerfile

import "os"

// RestrictToUser is a no-op off Windows: the file is created with mode 0600.
func RestrictToUser(string) error { return nil }

// OnlyUser reports whether path is closed to group and others (mode 0600).
func OnlyUser(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode().Perm()&0o077 == 0, nil
}
