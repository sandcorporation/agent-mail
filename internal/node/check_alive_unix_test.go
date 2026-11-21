//go:build !windows

package node_test

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid runs; a zombie (an orphan nobody reaped
// yet) counts as ended.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return !os.IsNotExist(err)
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}
