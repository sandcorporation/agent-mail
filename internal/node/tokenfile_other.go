//go:build !windows

package node

// restrictToUser is a no-op off Windows: the file is created with mode 0600.
func restrictToUser(string) error { return nil }
