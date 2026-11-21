//go:build windows

package node_test

import "github.com/sandcorporation/switchboard/internal/winjob"

func processAlive(pid int) bool { return winjob.Alive(pid) }
