//go:build windows

package pty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type sysSession struct {
	hpc    windows.Handle
	in     *os.File // our write end of the terminal input
	out    *os.File // our read end of the terminal output
	proc   windows.Handle
	job    windows.Handle
	procID int

	pcOnce sync.Once
}

func startSys(s *Session, spec Spec) (err error) {
	app, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return err
	}

	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return fmt.Errorf("pty: input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return fmt.Errorf("pty: output pipe: %w", err)
	}
	u := &s.sys
	u.in = os.NewFile(uintptr(inW), "conpty-in")
	u.out = os.NewFile(uintptr(outR), "conpty-out")
	// The pseudo console keeps its own copies of the far pipe ends.
	defer windows.CloseHandle(inR)
	defer windows.CloseHandle(outW)
	defer func() {
		if err != nil {
			_ = u.close()
		}
	}()

	if err = windows.CreatePseudoConsole(coord(spec.Cols, spec.Rows), inR, outW, 0, &u.hpc); err != nil {
		return fmt.Errorf("pty: CreatePseudoConsole: %w", err)
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it.
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&u.hpc)), unsafe.Sizeof(u.hpc)); err != nil {
		return fmt.Errorf("pty: attach pseudo console: %w", err)
	}

	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// Without STARTF_USESTDHANDLES the child can pick up the parent's
	// redirected stdio instead of the pseudo console.
	si.Flags = windows.STARTF_USESTDHANDLES

	appPtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return err
	}
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(spec.Argv))
	if err != nil {
		return err
	}
	var dir *uint16
	if spec.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(spec.Dir); err != nil {
			return err
		}
	}
	envBlock, err := environmentBlock(spec.Env)
	if err != nil {
		return err
	}

	u.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("pty: CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(u.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("pty: SetInformationJobObject: %w", err)
	}

	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err = windows.CreateProcess(appPtr, cmdLine, nil, nil, false, flags, &envBlock[0], dir, &si.StartupInfo, &pi); err != nil {
		return fmt.Errorf("pty: CreateProcess: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	u.proc = pi.Process
	u.procID = int(pi.ProcessId)
	// Join the job before the first instruction runs so every descendant
	// lands in it too.
	if err = windows.AssignProcessToJobObject(u.job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return fmt.Errorf("pty: AssignProcessToJobObject: %w", err)
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateJobObject(u.job, 1)
		return fmt.Errorf("pty: ResumeThread: %w", err)
	}
	return nil
}

// environmentBlock builds a UTF-16 block of NUL-terminated KEY=VALUE strings
// ending in an extra NUL. An empty env still yields a valid (empty) block,
// so the child inherits nothing.
func environmentBlock(env []string) ([]uint16, error) {
	var b []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, errors.New("pty: NUL in Env")
		}
		b = append(b, windows.StringToUTF16(kv)...)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0), nil
}

func coord(cols, rows uint16) windows.Coord {
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

func (u *sysSession) read(p []byte) (int, error) {
	n, err := u.out.Read(p)
	if err != nil && (errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, os.ErrClosed)) {
		err = io.EOF
	}
	return n, err
}

func (u *sysSession) write(p []byte) (int, error) { return u.in.Write(p) }

func (u *sysSession) resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(u.hpc, coord(cols, rows))
}

func (u *sysSession) pid() int { return u.procID }

func (u *sysSession) wait() (int, error) {
	if _, err := windows.WaitForSingleObject(u.proc, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(u.proc, &code); err != nil {
		return -1, err
	}
	// ConPTY keeps the output pipe open after the child exits; closing the
	// pseudo console flushes it and lets Read reach EOF. It can block until
	// the output is read, so it runs on its own goroutine.
	go u.closePC()
	return int(code), nil
}

func (u *sysSession) closePC() {
	u.pcOnce.Do(func() {
		if u.hpc != 0 {
			windows.ClosePseudoConsole(u.hpc)
		}
	})
}

// interrupt types Ctrl-C into the terminal.
func (u *sysSession) interrupt() { _, _ = u.in.Write([]byte{0x03}) }

func (u *sysSession) kill() {
	if u.job != 0 {
		_ = windows.TerminateJobObject(u.job, 1)
	}
}

func (u *sysSession) close() error {
	u.closePC()
	if u.in != nil {
		u.in.Close()
	}
	if u.out != nil {
		u.out.Close()
	}
	if u.proc != 0 {
		windows.CloseHandle(u.proc)
	}
	if u.job != 0 {
		// KILL_ON_JOB_CLOSE ends anything still in the job.
		windows.CloseHandle(u.job)
	}
	return nil
}
