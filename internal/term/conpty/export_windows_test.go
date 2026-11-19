//go:build windows

package conpty

import "time"

// SetExitDelay makes Run, in this process, return d late with the log file
// still open: a keeper whose process outlives its pipe.
func SetExitDelay(d time.Duration) { runExit = func() { time.Sleep(d) } }
