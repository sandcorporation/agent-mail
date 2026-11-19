//go:build !windows

package server

// logConsoleEvents: console control events exist on Windows only; signals
// are logged by run.
func logConsoleEvents() {}
