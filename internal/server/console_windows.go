//go:build windows

package server

import (
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/windows"
)

// Console control events (wincon.h).
var consoleEvents = map[uintptr]string{
	0: "CTRL_C_EVENT",
	1: "CTRL_BREAK_EVENT",
	2: "CTRL_CLOSE_EVENT",
	5: "CTRL_LOGOFF_EVENT",
	6: "CTRL_SHUTDOWN_EVENT",
}

func consoleEventName(ev uintptr) string {
	if n, ok := consoleEvents[ev]; ok {
		return n
	}
	return fmt.Sprintf("console event %d", ev)
}

// logConsoleEvents logs every console control event by name before the Go
// runtime's handler (registered earlier, so called after this one) turns
// it into SIGINT or SIGTERM. For close, logoff and shutdown Windows ends
// the process a few seconds later whatever the program does, so this line
// may be the last one.
func logConsoleEvents() {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	cb := windows.NewCallback(func(ev uintptr) uintptr {
		log.Printf("console event %s", consoleEventName(ev))
		_ = os.Stderr.Sync()
		return 0 // not handled: on to the next handler
	})
	if r, _, err := proc.Call(cb, 1); r == 0 {
		log.Printf("console events: not logged: %v", err)
	}
}
