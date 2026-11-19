//go:build !windows

package server

import (
	"fmt"
	"os"
	"runtime"
)

// Keeper is the hidden `switchboard __keeper` command, which exists on
// Windows only: tmux keeps the sessions elsewhere.
func Keeper(string, []string) int {
	fmt.Fprintf(os.Stderr, "__keeper runs on Windows only, not %s\n", runtime.GOOS)
	return 2
}
