//go:build windows

package pty

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func readUntil(t *testing.T, s *Session, want string) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		p := make([]byte, 4096)
		for {
			n, err := s.Read(p)
			b.Write(p[:n])
			if strings.Contains(b.String(), want) || err != nil {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, want) {
			t.Fatalf("output %q lacks %q", s, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func cmdEnv() []string {
	return []string{`SystemRoot=C:\Windows`, `PATH=C:\Windows\System32`}
}

func TestWindowsEchoAndExitCode(t *testing.T) {
	s, err := Start(context.Background(), Spec{Argv: []string{"cmd.exe", "/c", "echo hello conpty & exit 7"}, Env: cmdEnv()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	readUntil(t, s, "hello conpty")
	code, err := s.Wait()
	if err != nil || code != 7 {
		t.Fatalf("Wait = %d, %v; want 7", code, err)
	}
	_, _ = io.Copy(io.Discard, s)
}

func TestWindowsResizeAndStop(t *testing.T) {
	s, err := Start(context.Background(), Spec{Argv: []string{"cmd.exe", "/k"}, Env: cmdEnv()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() { _, _ = io.Copy(io.Discard, s) }()
	if err := s.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := s.Stop(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Stop returned before the child exited")
	}
}
