package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/dashboard"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
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
	if rest, ok := strings.CutPrefix(path, "nodes"); ok && s.hostingHub().AdminNodesAPI(w, r, strings.TrimPrefix(rest, "/")) {
		return
	}
	hostingStub(w)
}

// assets serves the dashboard's vendored files (the terminal view's
// xterm.js) while hosting is on; with it off they are an unknown path.
func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	if !s.hosting {
		s.root(w, r)
		return
	}
	dashboard.Assets(w, r)
}

// hostingHub is the node channel over this server's database; its changes
// publish the live kind "terminals".
func (s *Server) hostingHub() *hosting.Hub {
	s.hubOnce.Do(func() {
		s.nodeHub = hosting.NewHub(s.hostingService(), s.live.Publish)
		a := s.termLimits
		a.Session = func(r *http.Request) (string, string, bool) {
			who, key, ok := s.adminAuth().Session(r)
			return who.User, key, ok
		}
		a.Origin = s.liveOriginAllowed
		s.nodeHub.SetAdmin(a)
		bosses := make([]string, 0, len(s.bossRoles))
		for r := range s.bossRoles {
			bosses = append(bosses, r)
		}
		sort.Strings(bosses)
		s.nodeHub.SetNotify(s.hostingNotify, bosses)
		s.nodeHub.SetSlotOf(s.hostingSlotOf)
	})
	return s.nodeHub
}

// hostingSlotOf is the hub's SlotOf, read from presence's slots: the slot
// of base at machine in directory or its session-marked form. Seen is the
// last register or heartbeat (the lease end minus one lease).
func (s *Server) hostingSlotOf(base, machine, directory string) (hosting.Slot, bool, error) {
	p := s.presence()
	v, ok, err := p.FolderSlot(base, machine, directory)
	if !ok || err != nil {
		return hosting.Slot{}, false, err
	}
	return hosting.Slot{Role: v.Role, Directory: v.Directory, SessionID: v.SessionID, LeaseUntil: v.LeaseExpiresAt,
		Seen: v.LeaseExpiresAt.Add(-p.LeaseLength()), Live: v.LeaseExpiresAt.After(s.now())}, true, nil
}

// hostingNotify is the hub's Notify: one system mail per role's mailbox in
// one transaction, idempotent by key (suffixed with the mailbox when there
// are several, since keys are unique per sender). A role without a mailbox
// is skipped.
func (s *Server) hostingNotify(to []string, subject, body, key string) error {
	var woke []string
	err := store.InTx(context.Background(), s.db, func(tx *sql.Tx) error {
		woke = woke[:0]
		seen := map[string]bool{}
		for _, role := range to {
			box, ok := identity.Mailbox(tx, role)
			if !ok || seen[box] {
				if !ok {
					log.Printf("hosting notice %s skipped for %q: not a registered role", key, role)
				}
				continue
			}
			seen[box] = true
			idem := key
			if len(to) > 1 {
				idem = key + ":" + box
			}
			if err := mail.InsertSystemMail(tx, box, subject, body, "", "normal", idem, s.now()); err != nil {
				return err
			}
			woke = append(woke, box)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, box := range woke {
		s.hub.Publish(box)
	}
	return nil
}

// hostingService is the hosted-session store over this server's database.
func (s *Server) hostingService() *hosting.Service {
	return hosting.New(s.db, s.now, func(base string) bool { return identity.RoleExists(s.db, base) })
}

// HostCLI is the binary's `host` command: the hosted-session records in
// DB_PATH, which must name this instance's own database (the same rule as
// SWITCHBOARD_HOSTING=on), so the operations database never gains records.
func HostCLI(action, id string, add wire.HostedSessionCreate, jsonOut bool, stdout io.Writer) (int, error) {
	return withInstanceDB("host", func(svc *hosting.Service) error { return svc.RunCLI(action, id, add, jsonOut, stdout) })
}

// NodeInviteCLI is the binary's `node invite NAME`: a one-time code for a new
// node, valid 15 minutes, against DB_PATH (the same rule as `host`). The code
// is printed once; the database keeps only its hash.
func NodeInviteCLI(name string, stdout io.Writer) (int, error) {
	return withInstanceDB("node invite", func(svc *hosting.Service) error {
		code, expires, err := svc.InviteNode(name)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\nnode %s: use once before %s with\n  switchboard node --host URL --join %s\n", code, name, expires.Local().Format("2006-01-02 15:04:05"), code)
		return err
	})
}

// withInstanceDB runs fn over the hosting service of DB_PATH, which must name
// this instance's own database, so the operations database never gains
// hosting rows.
func withInstanceDB(cmd string, fn func(*hosting.Service) error) (int, error) {
	path := os.Getenv("DB_PATH")
	switch {
	case path == "":
		return 0, errors.New(cmd + ": DB_PATH is unset; name this instance's own database")
	case filepath.ToSlash(filepath.Clean(path)) == containerDBPath:
		return 0, errors.New(cmd + ": DB_PATH is the container default " + containerDBPath + "; name this instance's own database")
	}
	db, err := store.Open(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err = fn(hosting.New(db, time.Now, func(base string) bool { return identity.RoleExists(db, base) })); err != nil {
		return 0, err
	}
	return 0, nil
}
