//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	xconpty "github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/term"
)

// proc is a child in a ConPTY. The library creates the pseudo-console and
// spawns into it; this type owns every handle afterwards and closes them
// itself (the library's Close would close the console-side pipe ends a
// second time, and those are closed right after creation so the output
// pipe reports EOF once the console goes).
type proc struct {
	pc      *xconpty.ConPty
	hpc     windows.Handle
	in, out windows.Handle // our ends of the input and output pipes
	process windows.Handle
	job     windows.Handle // this session's tree
	pid     int

	lastOut  atomic.Int64 // unix nanoseconds of the last output
	pcOnce   sync.Once
	readOnce sync.Once
	readErr  error
	closed   atomic.Bool
}

// Spawn is the keeper's Spawner on Windows: one ConPTY of the Spec's size,
// the child created in it with exactly the Spec's environment, and the child
// placed in a job of its own (inside the keeper's job) so Stop ends its
// whole tree.
func Spawn(s term.Spec) (Proc, error) {
	cl, err := ResolveCommand(s.Argv, exec.LookPath, os.Getenv("ComSpec"), windows.EscapeArg)
	if err != nil {
		return nil, err
	}
	pc, err := xconpty.New(s.Cols, s.Rows, 0)
	if err != nil {
		return nil, err
	}
	p := &proc{
		pc: pc, hpc: windows.Handle(pc.Fd()),
		in: windows.Handle(pc.InPipeWriteFd()), out: windows.Handle(pc.OutPipeReadFd()),
	}
	// The console holds its own duplicates of its pipe ends.
	_ = windows.CloseHandle(windows.Handle(pc.InPipeReadFd()))
	_ = windows.CloseHandle(windows.Handle(pc.OutPipeWriteFd()))
	p.lastOut.Store(time.Now().UnixNano())

	env := s.Env
	if env == nil {
		env = []string{}
	}
	attr := &syscall.ProcAttr{Dir: s.Dir, Env: env, Sys: &syscall.SysProcAttr{CmdLine: cl.Line}}
	pid, h, err := pc.Spawn(cl.Program, s.Argv, attr)
	if err != nil {
		p.closeConsole()
		p.closePipes()
		return nil, err
	}
	p.pid, p.process = pid, windows.Handle(h)
	if p.job, err = newJob(); err == nil {
		err = windows.AssignProcessToJobObject(p.job, p.process)
	}
	if err != nil {
		// The keeper's job still holds the child.
		fmt.Fprintf(os.Stderr, "keeper: %s: session job: %v\n", s.Name, err)
	}
	return p, nil
}

// newJob returns a job that kills its processes when its last handle closes.
func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func (p *proc) PID() int { return p.pid }

func (p *proc) Read(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, os.ErrClosed
	}
	var n uint32
	err := windows.ReadFile(p.out, b, &n, nil)
	if n > 0 {
		p.lastOut.Store(time.Now().UnixNano())
	}
	if err != nil {
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
			err = io.EOF
		}
		return int(n), err
	}
	return int(n), nil
}

func (p *proc) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.in, b, &n, nil)
	return int(n), err
}

func (p *proc) Resize(cols, rows int) error { return p.pc.Resize(cols, rows) }

// quietFor and quietMax bound how long Wait lets the console flush the
// child's last output before closing it.
const (
	quietFor = 200 * time.Millisecond
	quietMax = 2 * time.Second
)

func (p *proc) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	err := windows.GetExitCodeProcess(p.process, &code)
	deadline := time.Now().Add(quietMax)
	for time.Now().Before(deadline) && time.Since(time.Unix(0, p.lastOut.Load())) < quietFor {
		time.Sleep(20 * time.Millisecond)
	}
	// Closing the console ends its output pipe: Read then reports EOF.
	p.closeConsole()
	return int(code), err
}

// Terminate closes the console, which sends the child CTRL_CLOSE_EVENT, and
// ends the session's job (its whole tree) if the child is still there after
// grace.
func (p *proc) Terminate(grace time.Duration) {
	go func() {
		p.closeConsole()
		ev, err := windows.WaitForSingleObject(p.process, uint32(grace.Milliseconds()))
		if err == nil && ev == windows.WAIT_OBJECT_0 {
			return
		}
		if p.job != 0 {
			_ = windows.TerminateJobObject(p.job, 1)
		}
		_ = windows.TerminateProcess(p.process, 1)
	}()
}

func (p *proc) closeConsole() {
	p.pcOnce.Do(func() { windows.ClosePseudoConsole(p.hpc) })
}

func (p *proc) closePipes() {
	if p.closed.Swap(true) {
		return
	}
	_ = windows.CloseHandle(p.in)
	_ = windows.CloseHandle(p.out)
}

// Close releases everything once the child has exited. Closing the session
// job kills whatever the child left running.
func (p *proc) Close() error {
	p.closeConsole()
	p.closePipes()
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
	}
	return windows.CloseHandle(p.process)
}
