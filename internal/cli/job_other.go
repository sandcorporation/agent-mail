//go:build !unix && !windows

package cli

import (
	"errors"
	"os"
	"os/exec"
)

// The detached runner is Linux, macOS and Windows only.
var errNoRunnerHere = errors.New("job run is not supported on this system")

var (
	stopSignals    = []os.Signal{os.Interrupt}
	ignoredSignals []os.Signal
)

const runnerCopies = false

func checkRunner() error                       { return errNoRunnerHere }
func startDetached(*exec.Cmd) error            { return errNoRunnerHere }
func startChild(*exec.Cmd) (childGroup, error) { return childGroup{}, errNoRunnerHere }
func stopRunner(int) error                     { return errNoRunnerHere }
func pidAlive(int) bool                        { return false }
func exitCodeOf(ps *os.ProcessState) int       { return ps.ExitCode() }

type childGroup struct{}

func (childGroup) stop(bool) error { return errNoRunnerHere }
func (childGroup) close()          {}
