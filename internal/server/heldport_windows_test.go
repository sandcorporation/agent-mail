//go:build windows

package server

import (
	"context"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

// boundPort holds a loopback port bound but not listening, the way the
// program that held 8090 on the first Windows run did (#217):
// Get-NetTCPConnection -State Listen shows nothing for it, and every bind of
// it fails until the test ends.
func boundPort(t *testing.T) string {
	t.Helper()
	h, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Closesocket(h) })
	if err := syscall.Bind(h, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(h)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(sa.(*syscall.SockaddrInet4).Port)
}

// backendRunning reports whether the keeper of instance answers on its pipe.
func backendRunning(_ []string, instance string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := conpty.DialFor(instance)(ctx)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
