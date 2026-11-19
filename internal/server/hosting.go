package server

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/config"
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
