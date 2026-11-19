//go:build !windows

package conpty

import "syscall"

// peerResets are the socket errors of a peer that closed with our data unread.
var peerResets = []error{syscall.ECONNRESET, syscall.ECONNABORTED}
