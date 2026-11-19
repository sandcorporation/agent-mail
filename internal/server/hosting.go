package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Terminal hosting (#173) is compiled in but off. SWITCHBOARD_HOSTING=on turns
// it on; the operations instance never sets it (internal/config guard test).
// While off, no hosting route, table, tab or live kind is observable.

const (
	defaultListenAddr = ":8080"
	containerDBPath   = "/data/mail.db" // the Dockerfile's DB_PATH
)

// hostingStart reads SWITCHBOARD_HOSTING and, when it is on, refuses an
// unsafe start: the listen address must be loopback, the admin mode must sign
// admins in (builtin or oidc), and DB_PATH must name a database other than
// the container default. The error names every failing condition.
func hostingStart(getenv func(string) string) (bool, error) {
	on, from := config.HostingFrom(getenv)
	if !on {
		return false, nil
	}
	var failed []string
	addr := getenv("LISTEN_ADDR")
	if addr == "" {
		addr = defaultListenAddr
	}
	if !loopbackAddr(addr) {
		failed = append(failed, "LISTEN_ADDR "+addr+" is not a loopback address (127.0.0.0/8, ::1 or localhost)")
	}
	if mode := getenv(config.EnvPrefix + "ADMIN_AUTH"); !adminauth.SignsIn(mode) {
		failed = append(failed, config.EnvPrefix+"ADMIN_AUTH must be builtin or oidc (got "+quoteOrUnset(mode)+")")
	}
	switch db := getenv("DB_PATH"); {
	case db == "":
		failed = append(failed, "DB_PATH is unset; name this instance's own database")
	case filepath.ToSlash(filepath.Clean(db)) == containerDBPath:
		failed = append(failed, "DB_PATH is the container default "+containerDBPath+"; name this instance's own database")
	}
	if len(failed) > 0 {
		return true, errors.New(from + "=on refused: " + strings.Join(failed, "; "))
	}
	return true, nil
}

func quoteOrUnset(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unset"
	}
	return `"` + v + `"`
}

// loopbackAddr reports whether a listen address binds only loopback. An empty
// host (":8080") binds every interface and is not loopback.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostingAdminPath reports whether an /api/admin/ path (prefix removed) is a
// hosting route: terminals… or nodes….
func hostingAdminPath(path string) bool {
	for _, p := range []string{"terminals", "nodes"} {
		if rest, ok := strings.CutPrefix(path, p); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			return true
		}
	}
	return false
}

// hostingNodePath reports whether an /api/ path (prefix removed) is the node
// channel: node/….
func hostingNodePath(path string) bool { return strings.HasPrefix(path, "node/") }

// hostingStub answers a hosting route while hosting is on; later tickets
// replace it with the real handlers.
func hostingStub(w http.ResponseWriter) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// hostingAdmin serves an /api/admin/ hosting path (prefix removed) for an
// identified admin past the write guard: the records and the node list, and
// the stub for the routes of later tickets.
func (s *Server) hostingAdmin(w http.ResponseWriter, r *http.Request, path string) {
	if rest, ok := strings.CutPrefix(path, "terminals"); ok && s.hostingHub().AdminTerminalsAPI(w, r, strings.TrimPrefix(rest, "/")) {
		return
	}
	if rest, ok := strings.CutPrefix(path, "nodes"); ok && s.hostingService().AdminNodesAPI(w, r, strings.TrimPrefix(rest, "/")) {
		return
	}
	hostingStub(w)
}

// hostingHub is the node channel over this server's database; its changes
// publish the live kind "terminals".
func (s *Server) hostingHub() *hosting.Hub {
	s.hubOnce.Do(func() { s.nodeHub = hosting.NewHub(s.hostingService(), s.live.Publish) })
	return s.nodeHub
}

// hostingService is the hosted-session store over this server's database.
func (s *Server) hostingService() *hosting.Service {
	return hosting.New(s.db, s.now, func(base string) bool { return identity.RoleExists(s.db, base) })
}

// HostCLI is the binary's `host` command: the hosted-session records in
// DB_PATH, which must name this instance's own database (the same rule as
// SWITCHBOARD_HOSTING=on), so the operations database never gains records.
func HostCLI(action, id string, add wire.HostedSessionCreate, jsonOut bool, stdout io.Writer) (int, error) {
	path := os.Getenv("DB_PATH")
	switch {
	case path == "":
		return 0, errors.New("host: DB_PATH is unset; name this instance's own database")
	case filepath.ToSlash(filepath.Clean(path)) == containerDBPath:
		return 0, errors.New("host: DB_PATH is the container default " + containerDBPath + "; name this instance's own database")
	}
	db, err := store.Open(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	svc := hosting.New(db, time.Now, func(base string) bool { return identity.RoleExists(db, base) })
	if err = svc.RunCLI(action, id, add, jsonOut, stdout); err != nil {
		return 0, err
	}
	return 0, nil
}
