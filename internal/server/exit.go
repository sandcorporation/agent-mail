package server

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
)

// Every way the server (and `up`) ends leaves a line in its log (#257): a
// start-up or serve error, a panic in one of its goroutines, a signal or a
// Windows console event. Only an outside kill (TerminateProcess, SIGKILL)
// ends it without one.

// osExit is os.Exit; tests replace it.
var osExit = os.Exit

// exitWith logs the exit code and why, flushes stderr (the log) and exits.
func exitWith(code int, why string) {
	log.Printf("exit %d: %s", code, why)
	_ = os.Stderr.Sync()
	osExit(code)
}

// fatal is log.Fatal with the exit line.
func fatal(v ...any) {
	log.Print(v...)
	exitWith(1, fmt.Sprint(v...))
}

// fatalf is log.Fatalf with the exit line.
func fatalf(format string, v ...any) {
	log.Printf(format, v...)
	exitWith(1, fmt.Sprintf(format, v...))
}

// goSafe runs fn in a goroutine named name; a panic in it is logged with
// its stack and ends the process with code 2, as an unrecovered panic does.
func goSafe(name string, fn func()) {
	go func() {
		defer recoverExit(name)
		fn()
	}()
}

// recoverExit, deferred, turns a panic into a log line with the stack and
// exit code 2.
func recoverExit(name string) {
	if p := recover(); p != nil {
		log.Printf("panic in %s: %v\n%s", name, p, debug.Stack())
		exitWith(2, "panic in "+name)
	}
}
