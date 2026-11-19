package server

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/node"
)

// openSetup opens the first-run admin setup of up's instance in dataDir:
// only in the builtin mode and only while there is no admin. The code is
// printed here once; the file keeps it, readable by this user only.
func (s *Server) openSetup(dataDir string) error {
	path := filepath.Join(dataDir, adminauth.SetupCodeFile)
	if s.admin.Mode != adminauth.ModeBuiltin {
		return removeIfExists(path)
	}
	st, code, err := adminauth.OpenSetup(s.db, path, node.WriteTokenFile)
	if err != nil || st == nil {
		return err
	}
	s.setup = st
	log.Printf("admin setup: open http://%s/setup and enter the code %s (also in %s)", config.Env("LISTEN_ADDR", defaultListenAddr), code, path)
	return nil
}

// setupAPI serves /setup and /setup/*; 404 unless up opened the setup.
func (s *Server) setupAPI(w http.ResponseWriter, r *http.Request) {
	s.adminAuth().SetupAPI(w, r, s.setup)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
