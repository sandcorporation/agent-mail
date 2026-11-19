//go:build windows

package conpty_test

// Temporary (#261): measures when the replaced and the shut-down keeper
// processes exit. Removed before merge.

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

func diagExit(t *testing.T, h windows.Handle) time.Time {
	t.Helper()
	if ev, err := windows.WaitForSingleObject(h, 30000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("process did not exit in 30 s: %v %v", ev, err)
	}
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		t.Fatal(err)
	}
	return time.Unix(0, e.Nanoseconds())
}

func TestDiag261KeeperExit(t *testing.T) {
	o := keeperOpts(t, "v1")
	c, _ := connect(t, o, "v1")
	if err := c.Launch(ctx(t), echoSpec("sb-a")); err != nil {
		t.Fatal(err)
	}
	k1, err := c.Version(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	h1, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(k1.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h1)
	setVersion(t, o, "v2")
	c2, how := connect(t, o, "v2")
	replaced := time.Now()
	if how != conpty.Replaced {
		t.Fatalf("keeper %s", how)
	}
	k2, err := c2.Version(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(k2.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h2)
	v1 := diagExit(t, h1).Sub(replaced)
	sc := conpty.NewClient(conpty.DialFor(o.Instance))
	cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shut := time.Now()
	_ = sc.Shutdown(cctx, time.Second)
	returned := time.Since(shut)
	v2 := diagExit(t, h2).Sub(shut)
	t.Logf("diag261 v1_exit_after_replaced_ms=%d v2_shutdown_call_ms=%d v2_exit_after_shutdown_ms=%d",
		v1.Milliseconds(), returned.Milliseconds(), v2.Milliseconds())
}
