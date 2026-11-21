package node

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

const (
	recA = "hs_0000000000000000000000000000000a"
	recB = "hs_0000000000000000000000000000000b"
)

func TestProfilesWriteHeaderAndPrune(t *testing.T) {
	p := &Profiles{Dir: filepath.Join(t.TempDir(), "switchboard"), Endpoint: "http://100.64.0.10:8090"} // leak-guard:allow
	if got := p.Header(); got != "-" {
		t.Errorf("empty header = %q", got)
	}
	if err := p.Write("../x", wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t"}); err == nil {
		t.Error("a bad record id was written")
	}
	for _, c := range []struct {
		rec string
		cr  wire.NodeCredential
	}{
		{recA, wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t1"}},
		{recA, wire.NodeCredential{ID: "rc_2", Role: "team/worker", Token: "t2"}}, // replaces
		{recB, wire.NodeCredential{ID: "rc_3", Role: "team/boss", Token: "t3"}},
	} {
		if err := p.Write(c.rec, c.cr); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.Header(); got != "rc_2,rc_3" {
		t.Errorf("header = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(p.Dir, "hosted-"+recA+".json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v %v", fi, err)
		}
	}
	// apply points held records at their profile and drops the rest.
	other := "hs_0000000000000000000000000000000c"
	recs := []Record{{ID: recA, Profile: "team-a"}, {ID: other, Profile: "mine"}}
	p.apply(recs)
	if recs[0].Profile != "hosted-"+recA || recs[1].Profile != "mine" {
		t.Errorf("profiles %q %q", recs[0].Profile, recs[1].Profile)
	}
	if p.Has(recB) {
		t.Error("the profile of an unassigned record stayed")
	}
	if got := p.Header(); got != "rc_2" {
		t.Errorf("header after prune = %q", got)
	}
}
