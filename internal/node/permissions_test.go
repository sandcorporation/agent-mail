package node_test

import (
	"slices"
	"testing"

	"github.com/sandcorporation/switchboard/internal/node"
)

var claudeSkip = []string{"--dangerously-skip-permissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`}

func TestArgvAddsTheAgentsPermissionSkip(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		skip  bool
		args  []string
		want  []string // between the session flag and the record's args
	}{
		{"claude skips by default", "/opt/agent/claude", true, nil, claudeSkip},
		{"asked", "/opt/agent/claude", false, nil, nil},
		{"windows shim", `C:\Users\me\AppData\Roaming\npm\claude.cmd`, true, []string{"--model", "opus"}, claudeSkip},
		{"windows exe", `C:\Users\me\.local\bin\Claude.EXE`, true, nil, claudeSkip},
		{"own permission mode", "/opt/agent/claude", true, []string{"--permission-mode", "plan"}, nil},
		{"own permission mode with =", "/opt/agent/claude", true, []string{"--permission-mode=plan"}, nil},
		{"own skip flag", "/opt/agent/claude", true, []string{"--dangerously-skip-permissions"}, nil},
		{"own allow flag", "/opt/agent/claude", true, []string{"--allow-dangerously-skip-permissions"}, nil},
		{"unknown agent", "/opt/agent/other-agent", true, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := node.Config{Agent: c.agent}
			r := node.Record{AgentSessionID: "conv-a", Args: c.args, SkipPermissions: c.skip}
			want := append(append([]string{c.agent, "--session-id", "conv-a"}, c.want...), c.args...)
			if got := cfg.Argv(r, false); !slices.Equal(got, want) {
				t.Fatalf("argv %q, want %q", got, want)
			}
		})
	}
}

// host set --skip-permissions on reaches a running session at its next
// restart: the record the node is assigned carries it, and the relaunch
// (resume) adds the arguments.
func TestSkipPermissionsAppliesAtTheNextRestart(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	r.reconcile(n)
	a.SkipPermissions = true
	n.Assign([]node.Record{a})
	r.reconcile(n)
	if got := r.launches(); len(got) != 1 {
		t.Fatalf("relaunched without a restart: %q", got)
	}
	a.RestartGen++
	n.Assign([]node.Record{a})
	r.reconcile(n)
	want := append(append([]string{"/opt/agent/claude", "--resume", "conv-a"}, claudeSkip...), "--model", "opus")
	if got := r.launches(); len(got) != 2 || !slices.Equal(got[1], want) {
		t.Fatalf("launches %q, want second %q", got, want)
	}
}
