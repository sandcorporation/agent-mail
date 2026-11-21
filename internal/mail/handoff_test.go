package mail

import "testing"

func TestHandoffTakesSessionMarkedFolder(t *testing.T) {
	w := Handoff{Base: "b", Machine: "m1", Directory: "/work/q", Label: "issue-1"}
	cases := []struct {
		machine, dir, label string
		want                bool
	}{
		{"m1", "/work/q", "issue-9", true},            // same identity, any label
		{"m1", "/work/q #52abcdef", "issue-1", true},  // marked form, same label
		{"m1", "/work/q #52abcdef", "", true},         // marked form, no label
		{"m1", "/work/q #52ABCDEF", "", true},         // hex in either case
		{"m1", "/work/q #52abcdef", "issue-2", false}, // marked form, another label
		{"m2", "/work/q #52abcdef", "", false},        // another machine
		{"m1", "/work/q #s2abcdef", "", false},        // not a session mark
		{"m1", "/work/q #52abcde", "", false},         // seven digits
		{"m1", "/work/qq #52abcdef", "", false},       // another folder
		{"m1", "/work/q2", "", false},                 // another folder
		{"m9", "/elsewhere", "issue-1", true},         // same label anywhere
	}
	for _, c := range cases {
		if got := w.takes(c.machine, c.dir, c.label, false); got != c.want {
			t.Errorf("takes(%q, %q, %q) = %v, want %v", c.machine, c.dir, c.label, got, c.want)
		}
	}
	marked := Handoff{Base: "b", Machine: "m1", Directory: "/work/q #52abcdef", Label: ""}
	if !marked.takes("m1", "/work/q", "issue-3", false) {
		t.Error("a session-marked asker's plain folder does not take its mail")
	}
}

// Mail a role sent (#354) is strict: no label does not match no label, so
// only the same folder or a real label takes it.
func TestHandoffStrictForRoleMail(t *testing.T) {
	w := Handoff{Base: "b", Machine: "m1", Directory: "/work/x"}
	if w.takes("m1", "/work/y", "", true) {
		t.Error("another folder without a label took role mail")
	}
	if !w.takes("m1", "/work/y", "", false) {
		t.Error("system mail: no label no longer matches no label")
	}
	if !w.takes("m1", "/work/x #52abcdef", "", true) {
		t.Error("the same folder did not take role mail")
	}
	l := Handoff{Base: "b", Machine: "m1", Directory: "/work/x", Label: "issue-1"}
	if !l.takes("m9", "/elsewhere", "issue-1", true) {
		t.Error("the same label did not take role mail")
	}
}
