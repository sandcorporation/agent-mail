//go:build windows

package conpty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// MinBuild is Windows 10 1809, the first build with ConPTY.
const MinBuild = 17763

// CheckWindows refuses Windows older than 10 1809.
func CheckWindows() error {
	v := windows.RtlGetVersion()
	if v.MajorVersion < 10 || v.MajorVersion == 10 && v.MinorVersion == 0 && v.BuildNumber < MinBuild {
		return fmt.Errorf("node: terminal hosting needs Windows 10 1809 (build %d) or later; this is %d.%d build %d",
			MinBuild, v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	return nil
}

// Run is the keeper process: it places itself in a job that kills every
// process in it when the keeper's handle closes (the children inherit the
// job, so none outlives the keeper), listens on the instance's pipe and
// serves until a client asks it to shut down.
func Run(o RunOptions, version string) error {
	logf := log.New(io.Discard, "", 0).Printf
	if o.LogFile != "" {
		f, err := os.OpenFile(o.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		logf = log.New(f, "", log.LstdFlags).Printf
	}
	job, err := newJob()
	if err != nil {
		return fmt.Errorf("keeper: job: %w", err)
	}
	defer windows.CloseHandle(job) // the keeper ends here: its job kills it
	defer runExit()
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		return fmt.Errorf("keeper: join job: %w", err)
	}
	l, err := Listen(o.Instance)
	if err != nil {
		return fmt.Errorf("keeper: listen %s: %w", PipeName(o.Instance), err)
	}
	k := NewKeeper(KeeperOptions{Spawn: Spawn, History: o.History, Version: version, Logf: logf})
	logf("keeper %s: listening on %s", version, PipeName(o.Instance))
	errc := make(chan error, 1)
	go func() { errc <- k.Serve(l) }()
	limit := o.IdleLimit
	if limit <= 0 {
		limit = DefaultIdleLimit
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := make(chan bool, 1)
	go func() { idle <- k.WatchIdle(ctx, limit, 0) }()
	select {
	case ok := <-idle:
		if ok {
			k.Shutdown(DefaultGrace)
		}
		return nil
	case <-k.Done():
		logf("keeper: shut down")
		return nil
	case err := <-errc:
		k.Shutdown(DefaultGrace)
		return err
	}
}

// runExit runs as Run returns, before the job ends the process. Tests
// replace it to make a keeper slow to exit.
var runExit = func() {}

// Start starts a keeper process (exe with o.Args()) that outlives the
// caller: detached from its console, in a process group of its own, and out
// of the caller's job when the job allows breaking away.
func Start(exe string, o RunOptions) error {
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	start := func(flags uint32) error {
		cmd := exec.Command(exe, o.Args()...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	err := start(flags | windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The caller's job forbids breakaway; the keeper then lives as long
		// as that job does.
		err = start(flags)
	}
	return err
}
