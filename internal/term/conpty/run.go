package conpty

import (
	"flag"
	"fmt"
	"io"
	"strconv"
)

// KeeperCommand is the hidden subcommand that runs the keeper.
const KeeperCommand = "__keeper"

// RunOptions configures a keeper process.
type RunOptions struct {
	// Instance names the pipe: one keeper per node instance.
	Instance string
	// History is the scrollback per session.
	History int
	// LogFile receives the keeper's log (empty: discarded).
	LogFile string
}

// Args is the command line (after the program) that starts a keeper.
func (o RunOptions) Args() []string {
	args := []string{KeeperCommand, "--instance", o.Instance, "--history", strconv.Itoa(o.History)}
	if o.LogFile != "" {
		args = append(args, "--log", o.LogFile)
	}
	return args
}

// ParseRunArgs reads the arguments after KeeperCommand.
func ParseRunArgs(args []string) (RunOptions, error) {
	var o RunOptions
	fs := flag.NewFlagSet(KeeperCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Instance, "instance", "", "")
	fs.IntVar(&o.History, "history", 0, "")
	fs.StringVar(&o.LogFile, "log", "", "")
	if err := fs.Parse(args); err != nil {
		return RunOptions{}, fmt.Errorf("%s: %w", KeeperCommand, err)
	}
	if o.Instance == "" || fs.NArg() > 0 {
		return RunOptions{}, fmt.Errorf("%s: usage: --instance ID [--history N] [--log FILE]", KeeperCommand)
	}
	return o, nil
}
