package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestWriteTokenFileIsUserOnlyAndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "node.token")
	if err := WriteTokenFile(path, "sbn_one"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi.Mode(), err)
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil || di.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %v %v", di.Mode(), err)
		}
	}
	if err := WriteTokenFile(path, "sbn_two"); err == nil {
		t.Fatal("an existing token file was replaced")
	}
	if got, err := ReadTokenFile(path); err != nil || got != "sbn_one" {
		t.Fatalf("read %q %v", got, err)
	}
	again, err := EnsureTokenFile(path)
	if err != nil || again != "sbn_one" {
		t.Fatalf("ensure on an existing file = %q %v", again, err)
	}
	fresh := filepath.Join(t.TempDir(), "node.token")
	made, err := EnsureTokenFile(fresh)
	if err != nil || !strings.HasPrefix(made, "sbn_") || len(made) != 4+64 {
		t.Fatalf("ensure = %q %v", made, err)
	}
}

func TestJoinPostsTheCodeAndReadsTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j wire.NodeJoin
		if r.Method != http.MethodPost || r.URL.Path != "/api/node/join" || json.NewDecoder(r.Body).Decode(&j) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if j.Code != "sbj_ok" {
			http.Error(w, "invite code is unknown or already used", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(wire.NodeJoined{Name: "laptop", Token: "sbn_t"})
	}))
	defer srv.Close()
	j, err := Join(context.Background(), srv.URL+"/", " sbj_ok\n")
	if err != nil || j.Name != "laptop" || j.Token != "sbn_t" {
		t.Fatalf("join = %+v %v", j, err)
	}
	if _, err := Join(context.Background(), srv.URL, "sbj_no"); err == nil || !strings.Contains(err.Error(), "unknown or already used") {
		t.Fatalf("refused join: %v", err)
	}
	if _, err := Join(context.Background(), "ftp://x", "c"); err == nil {
		t.Fatal("ftp host accepted")
	}
}
