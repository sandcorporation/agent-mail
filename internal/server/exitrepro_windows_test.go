//go:build windows

package server

// Temporary (#257): console control events reaching `up` in its own console.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestExitReproConsole(t *testing.T) {
	if h := os.Getenv("SB_CON_HELPER"); h != "" {
		conHelper(h)
		return
	}
	if os.Getenv("SB_EXIT_REPRO") == "" {
		t.Skip("temporary repro: SB_EXIT_REPRO=1")
	}
	root, err := os.MkdirTemp("", "sbcon")
	if err != nil {
		t.Fatal(err)
	}
	bin := e2eBinary(t, filepath.Join(root, "bin"))
	for _, what := range []string{"break", "close", "early"} {
		data := filepath.Join(root, what)
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		ln.Close()
		logf, _ := os.Create(filepath.Join(root, what+".log"))
		cmd := exec.Command(bin, "up", "--data", data, "--port", port)
		cmd.Env = append(cliEnv(), "SWITCHBOARD_ADMIN_AUTH=builtin", "SWITCHBOARD_HOSTING_AGENT="+os.Getenv("ComSpec"))
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		// up binds its port before the keeper; wait until it serves.
		for i := 0; i < 150 && what != "early"; i++ {
			if b, _ := os.ReadFile(filepath.Join(root, what+".log")); strings.Contains(string(b), "node: attached") {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		h := exec.Command(os.Args[0], "-test.run=^TestExitReproConsole$")
		h.Env = append(os.Environ(), "SB_CON_HELPER="+strconv.Itoa(cmd.Process.Pid)+" "+what)
		hout, herr := h.CombinedOutput()
		var res string
		select {
		case err := <-done:
			res = fmt.Sprintf("up ended: %v (exit code %d)", err, cmd.ProcessState.ExitCode())
		case <-time.After(30 * time.Second):
			res = "up still running after 30 s"
			_ = cmd.Process.Kill()
			<-done
		}
		logf.Close()
		out, _ := os.ReadFile(filepath.Join(root, what+".log"))
		var keep []string
		for _, l := range strings.Split(string(out), "\n") {
			if !strings.Contains(l, "admin setup") {
				keep = append(keep, l)
			}
		}
		t.Logf("RESULT ctrl-%s: %s\nhelper: %v %s\nup output:\n%s", what, res, herr, hout, strings.Join(keep, "\n"))
	}
	// End the keepers: TerminateProcess every __keeper this test started.
	_ = exec.Command("powershell.exe", "-NoProfile", "-Command", "Get-CimInstance Win32_Process -Filter \"Name = 'switchboard.exe'\" | Where-Object { $_.CommandLine -match 'sbcon' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }").Run()
}

func conHelper(arg string) {
	f := strings.Fields(arg)
	pid, _ := strconv.Atoi(f[0])
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	u32 := windows.NewLazySystemDLL("user32.dll")
	call := func(dll *windows.LazyDLL, name string, a ...uintptr) uintptr {
		r, _, err := dll.NewProc(name).Call(a...)
		fmt.Printf("%s%v = %d (%v)\n", name, a, r, err)
		return r
	}
	call(k32, "FreeConsole")
	call(k32, "AttachConsole", uintptr(pid))
	if f[1] != "close" {
		call(k32, "SetConsoleCtrlHandler", 0, 1)
		call(k32, "GenerateConsoleCtrlEvent", 1, 0)
	} else {
		w := call(k32, "GetConsoleWindow")
		call(u32, "PostMessageW", w, 0x10, 0, 0)
	}
	os.Exit(0)
}
