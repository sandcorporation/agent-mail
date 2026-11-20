//go:build windows

package winjob

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procIsProcessInJob = kernel32.NewProc("IsProcessInJob")
	ntdll              = windows.NewLazySystemDLL("ntdll.dll")
	procNtResume       = ntdll.NewProc("NtResumeProcess")
)

// stillActive is GetExitCodeProcess's code for a process that has not ended.
const stillActive = 259

// New returns a job object. killOnClose ends its processes when its last
// handle closes; breakawayOK lets them start children outside it.
func New(killOnClose, breakawayOK bool) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if killOnClose {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if breakawayOK {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// Start starts cmd suspended, places it in job and only then lets it run,
// so nothing it starts escapes the job. cmd.SysProcAttr keeps its other
// settings. When the process cannot be placed it is killed.
func Start(cmd *exec.Cmd, job windows.Handle) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(fmt.Errorf("open process: %w", err))
	}
	defer windows.CloseHandle(h)
	if err = windows.AssignProcessToJobObject(job, h); err != nil {
		return fail(fmt.Errorf("assign to job: %w", err))
	}
	if st, _, _ := procNtResume.Call(uintptr(h)); st != 0 {
		return fail(fmt.Errorf("resume: NTSTATUS %#x", st))
	}
	return nil
}

// InJob reports whether this process belongs to a job object.
func InJob() (bool, error) {
	var in int32
	r, _, err := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&in)))
	if r == 0 {
		return false, err
	}
	return in != 0, nil
}

// ErrNoBreakaway: this process is in a job that does not let the processes
// it starts leave it.
var ErrNoBreakaway = errors.New("the parent job does not allow breakaway")

// CheckBreakaway returns ErrNoBreakaway when a process started with
// CREATE_BREAKAWAY_FROM_JOB would stay in this process's job (and end with
// it). It reads the immediate job only; a start can still be refused by an
// outer one, which then fails with ERROR_ACCESS_DENIED.
func CheckBreakaway() error {
	in, err := InJob()
	if err != nil || !in {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if err = windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return err
	}
	if info.BasicLimitInformation.LimitFlags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) == 0 {
		return ErrNoBreakaway
	}
	return nil
}

// ProbeBreakaway starts exe suspended the way a runner starts
// (CREATE_BREAKAWAY_FROM_JOB) and ends it at once. It catches what
// CheckBreakaway cannot see: an outer job that forbids breakaway, which
// refuses the start with ERROR_ACCESS_DENIED (ErrNoBreakaway here).
func ProbeBreakaway(exe string) error {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_SUSPENDED,
		HideWindow:    true,
	}
	err := cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w: %v", ErrNoBreakaway, err)
	}
	if err != nil {
		return err
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil
}

// Alive reports whether process pid is still running.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// A process we may not open still exists.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
