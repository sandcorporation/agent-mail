//go:build unix

package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
)

// TestNodeRelaunchesWhenTheServerIsGone kills the tmux server under a
// running node: every session comes back with --resume of its own
// conversation.
func TestNodeRelaunchesWhenTheServerIsGone(t *testing.T) {
	b := newBackend(t, 100)
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	agent := filepath.Join(dir, "agent")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\nexec sleep 600\n"
	if err := os.WriteFile(agent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Config{Agent: agent, Machine: "pc-1", Endpoint: "http://127.0.0.1:1", Environ: os.Environ()}, b)
	if err != nil {
		t.Fatal(err)
	}
	recs := []node.Record{
		{ID: "a", Profile: "p", Directory: dir, AgentSessionID: "conv-a", Restart: node.RestartAlways, Desired: node.DesiredRunning},
		{ID: "b", Profile: "p", Directory: dir, AgentSessionID: "conv-b", Restart: node.RestartAlways, Desired: node.DesiredRunning},
	}
	n.Assign(recs)
	ctx := context.Background()
	lines := func() []string {
		b, _ := os.ReadFile(log)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
	}
	waitLines := func(want int) []string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := n.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if l := lines(); len(l) >= want {
				return l
			}
			if time.Now().After(deadline) {
				t.Fatalf("launches %q, want %d", lines(), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	got := waitLines(2)
	if strings.Join(got, " ") != "--session-id_conv-a --session-id_conv-b" {
		t.Fatalf("first launches %q", got)
	}

	if err := b.command(nil, "kill-server").Run(); err != nil {
		t.Fatal(err)
	}
	got = waitLines(4)
	if strings.Join(got[2:], " ") != "--resume_conv-a --resume_conv-b" {
		t.Fatalf("relaunches %q", got)
	}
	for _, s := range n.States() {
		if s.Run != 2 || s.Observed == node.ObservedBackingOff || s.Observed == node.ObservedCrashLoop {
			t.Fatalf("state %+v", s)
		}
	}
	infos, err := b.List(ctx)
	if err != nil || len(infos) != 2 || !infos[0].Alive || !infos[1].Alive {
		t.Fatalf("sessions after relaunch %+v %v", infos, err)
	}
}
