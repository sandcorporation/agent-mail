// Package server assembles the mail server: the Server settings read from
// the environment, the HTTP router with its role-auth and admin guards
// (server.go), the per-request domain services (services.go) and the
// background sweep (stall.go). Domain packages never import it.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/asks"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/dashboard"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/live"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

type Server struct {
	db          *sql.DB
	tokens      map[string]string
	hub         mail.Hub            // wakes event streams and long polls (internal/mail)
	mailCfg     mail.Config         // SSE keep-alive and long-poll cap (internal/mail)
	done        chan struct{}       // closed on shutdown; ends event streams
	presenceCfg presence.Config     // slot lease and presence settings (internal/presence)
	clock       func() time.Time    // nil means time.Now; tests move time forward
	sweepEvery  time.Duration       // SWITCHBOARD_SWEEP_INTERVAL; 0 means defaultSweepInterval
	labelStall  time.Duration       // SWITCHBOARD_LABEL_STALL; 0 means defaultLabelStall
	handoffTTL  time.Duration       // SWITCHBOARD_HANDOFF_EXPIRE; 0 means defaultHandoffTTL (#354)
	asksCfg     asks.Config         // permission TTL (internal/asks)
	bossRoles   map[string]bool     // SWITCHBOARD_BOSS_ROLES; may answer any permission request
	bossOrder   []string            // SWITCHBOARD_BOSS_ROLES in listed order (identity.Service.AskVia)
	discord     *insight.Discord    // SWITCHBOARD_DISCORD_WEBHOOK; nil sends no question notices
	alerts      *insight.Thresholds // SWITCHBOARD_ALERT_*; nil means insight.DefaultThresholds
	oldHeaders  config.AliasCounter // requests that used an old-prefix header
	groups      identity.Groups     // collaborator groups (internal/identity)
	admin       adminauth.Config    // admin login settings (internal/adminauth)
	loginLimits adminauth.Limiter   // failed admin logins
	joinLimits  adminauth.Limiter   // failed POST /api/join per client IP (identity.Service.Join)
	live        live.Bus            // dashboard change stream (internal/live, served by live.go)
	publicURL   string              // SWITCHBOARD_PUBLIC_URL; a WebSocket Origin it also accepts
	setup       *adminauth.Setup    // first-run admin setup; only up opens it (setup.go), nil answers 404
	hosting     bool                // SWITCHBOARD_HOSTING=on (hosting.go); off answers every hosting route 404
	hubOnce     sync.Once
	nodeHub     *hosting.Hub  // the node channel (hosting.go), built on first use
	termLimits  hosting.Admin // browser terminal recheck and queue; tests shorten them (zero: the defaults)
}

// version is the build version /health reports (set by Run).
var version = "dev"

// Run reads the settings from the environment, opens the database and serves
// until SIGINT or SIGTERM. v is the build version.
func Run(v string) {
	logConsoleEvents()
	run(v, nil, "", nil)
}

// attachFunc starts more work on a server about to serve (up's node); ctx
// ends at shutdown.
type attachFunc func(ctx context.Context, s *Server) error

// run is Run; ln, when set, is the listener already bound to LISTEN_ADDR
// (up binds it first), setupDir, when set, is up's data directory, where the
// first-run admin setup keeps its code, and attach, when set, runs after the
// listener is bound.
func run(v string, ln net.Listener, setupDir string, attach attachFunc) {
	version = v
	hostingOn, e := hostingStart(os.Getenv)
	if e != nil {
		fatal(e)
	}
	db, e := store.Open(config.Env("DB_PATH", "/data/mail.db"))
	if e != nil {
		fatal(e)
	}
	s := &Server{db: db, tokens: map[string]string{}, admin: adminauth.Config{ProxyToken: config.EnvAlias("ADMIN_PROXY_TOKEN")}, done: make(chan struct{}), publicURL: strings.TrimRight(config.EnvAlias("PUBLIC_URL"), "/"), hosting: hostingOn}
	if v, from := config.EnvAliasFrom(os.Getenv, "SSE_KEEPALIVE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fatal(from + " must be a positive duration such as 20s")
		}
		s.mailCfg.KeepAlive = d
	}
	if v, from := config.EnvAliasFrom(os.Getenv, "POLL_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fatal(from + " must be a positive whole number of seconds such as 55")
		}
		s.mailCfg.PollMax = time.Duration(n) * time.Second
	}
	if v, from := config.EnvAliasFrom(os.Getenv, "SLOT_LEASE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			fatal(from + " must be a duration of at least 1s such as 3m")
		}
		s.presenceCfg.SlotLease = d
	}
	if e = presence.Configure(&s.presenceCfg, os.Getenv); e != nil {
		fatal(e)
	}
	configureCLIMin(s, os.Getenv, version, log.Printf)
	if e = configureStall(s, os.Getenv); e != nil {
		fatal(e)
	}
	if e = configureAlerts(s, os.Getenv); e != nil {
		fatal(e)
	}
	s.discord = insight.ConfigureDiscord(os.Getenv)
	if e = adminauth.Configure(&s.admin, os.Getenv); e != nil {
		fatal(e)
	}
	if e = s.logRemindStart(); e != nil {
		fatal(e)
	}
	if setupDir != "" {
		if e = s.openSetup(setupDir); e != nil {
			fatal(e)
		}
	}
	for _, pair := range strings.Split(config.EnvAlias("TOKENS"), ",") {
		p := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(p) == 2 && p[0] != "" && p[1] != "" {
			s.tokens[p[0]] = p[1]
		}
	}
	if e = migrateConfiguredRoles(s); e != nil {
		fatal("cannot migrate configured roles")
	}
	if e = s.ids().LoadGroups(); e != nil {
		fatal("cannot load groups")
	}
	if e = s.migrateAnsweredRule(log.Printf); e != nil {
		fatalf("answered rule: %v", e)
	}
	if old := config.OldEnvNames(); len(old) > 0 {
		log.Printf("old environment names in use (rename AGENT_MAIL_* to SWITCHBOARD_*): %s", strings.Join(old, ", "))
	}
	if s.hosting {
		log.Printf("terminal hosting is on")
	}
	addr := config.Env("LISTEN_ADDR", defaultListenAddr)
	// No WriteTimeout: /api/events responses stay open indefinitely and
	// /api/events/poll waits up to SWITCHBOARD_POLL_MAX.
	srv := &http.Server{Addr: addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	srv.RegisterOnShutdown(func() { close(s.done) })
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	stopped := make(chan struct{})
	waitSweeper := s.startSweeper()
	if ln == nil {
		if ln, e = net.Listen("tcp", addr); e != nil {
			fatal(e)
		}
	}
	// The remote listener (#329) serves nodes on other machines.
	var remote *http.Server
	if s.hosting {
		rs, _ := hostingRemote(os.Getenv) // checked by hostingStart
		if rs.addr != "" {
			rln, err := net.Listen("tcp", rs.addr)
			if err != nil {
				fatal(err)
			}
			remote = &http.Server{Handler: s.remoteHandler(rs.allow), ReadHeaderTimeout: 10 * time.Second}
			log.Printf("switchboard remote listener on %s (node channel and role API; peers in %s)", rs.addr, prefixList(rs.allow))
			goSafe("remote listener", func() {
				if err := remote.Serve(rln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fatal(err)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if s.hosting {
		goSafe("hosting hub", func() { s.hostingHub().Run(ctx, hosting.SyncInterval) })
	}
	if attach != nil {
		if e = attach(ctx, s); e != nil {
			fatal(e)
		}
	}
	var why string
	goSafe("shutdown", func() {
		sig := <-stop
		why = "stopped by signal " + sig.String()
		log.Printf("signal %s: shutting down", sig)
		cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if remote != nil {
			_ = remote.Shutdown(ctx)
		}
		close(stopped)
	})
	log.Printf("switchboard listening on %s", addr)
	if e = srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
		fatal(e)
	}
	<-stopped
	waitSweeper()
	_ = db.Close()
	log.Printf("exit 0: %s", why)
	_ = os.Stderr.Sync()
}

// handler is the server's HTTP handler: health, API, admin sign-in and the
// dashboard page, behind the security headers.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/auth/", s.authAPI)
	mux.HandleFunc("/setup", s.setupAPI)
	mux.HandleFunc("/setup/", s.setupAPI)
	mux.HandleFunc(dashboard.AssetsPrefix, s.assets)
	mux.HandleFunc("/", s.root)
	return secureHeaders(mux)
}

// configureAlerts reads the metrics alert thresholds.
func configureAlerts(s *Server, getenv func(string) string) error {
	t, err := insight.ParseThresholds(getenv)
	if err != nil {
		return err
	}
	s.alerts = &t
	return nil
}

func migrateConfiguredRoles(s *Server) error {
	for role, token := range s.tokens {
		if identity.Reserved(role) {
			log.Printf("SWITCHBOARD_TOKENS: %s is reserved for system notices; ignored", role)
			continue
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO roles(role,token,created_at) VALUES(?,?,?)`, role, store.TokenHash(token), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// adminAuth is admin login over this server's settings, database and clock.
// Its POST /auth/* endpoints need the same write guard as admin writes.
func (s *Server) adminAuth() *adminauth.Service {
	return adminauth.New(&s.admin, s.db, s.now, s.header, func(r *http.Request) bool {
		return adminWriteAllowed(r, s.header(r, "Dashboard"))
	}, &s.loginLimits)
}

// authAPI serves /auth/ (admin login and logout, OIDC).
func (s *Server) authAPI(w http.ResponseWriter, r *http.Request) { s.adminAuth().AuthAPI(w, r) }

// root serves the dashboard page or the login page.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	s.adminAuth().Root(w, r, dashboard.Page)
}

// auth returns the mailbox name of an authenticated request (see authenticate).
func (s *Server) auth(r *http.Request) (string, bool) {
	p, ok := s.authenticate(r)
	return p.Role, ok
}

// serveAPI routes /api/ (api publishes the live change around it).
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		s.adminAPI(w, r)
		return
	}
	if p := strings.TrimPrefix(r.URL.Path, "/api/"); s.hosting && hostingNodePath(p) {
		s.hostingHub().NodeAPI(w, r, strings.TrimPrefix(p, "node/"))
		return
	}
	if r.URL.Path == "/api/register" {
		s.ids().Register(w, r)
		return
	}
	if r.URL.Path == "/api/join" {
		s.ids().Join(w, r, s.adminAuth().ClientIP(r), &s.joinLimits)
		return
	}
	who, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	role := who.Role
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if path == "permissions" || strings.HasPrefix(path, "permissions/") {
		s.asks().PermissionAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "permissions"), "/"))
		return
	}
	if path == "questions" || strings.HasPrefix(path, "questions/") {
		s.asks().QuestionAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "questions"), "/"))
		return
	}
	if path == "decisions" {
		s.asks().DecisionsAPI(w, r, &who)
		return
	}
	if path == "presence" {
		s.presence().API(w, r, &who)
		return
	}
	if path == "whoami" {
		s.ids().WhoamiAPI(w, r, who)
		return
	}
	if path == "invites" {
		s.ids().InvitesAPI(w, r, who)
		return
	}
	if path == "metrics" {
		s.insight().MetricsAPI(w, r, s.ids().IsGlobalBoss(who), false)
		return
	}
	if path == "progress" {
		s.progressAPI(w, r, s.ids().IsGlobalBoss(who))
		return
	}
	if path == "reports" {
		s.reports().API(w, r, who)
		return
	}
	if path == "jobs" || strings.HasPrefix(path, "jobs/") {
		s.jobs().API(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "jobs"), "/"))
		return
	}
	if path == "schedules" || strings.HasPrefix(path, "schedules/") {
		s.jobs().ScheduleAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "schedules"), "/"))
		return
	}
	if path == "slots" || strings.HasPrefix(path, "slots/") {
		s.presence().SlotAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "slots"), "/"))
		return
	}
	if path == "messages" {
		switch r.Method {
		case "POST":
			s.mail().Send(w, r, who)
		case "GET":
			if of := strings.TrimSpace(r.URL.Query().Get("of")); of != "" {
				if !s.ids().IsGlobalBoss(who) {
					http.Error(w, "viewing another mailbox is for boss roles only", 403)
					return
				}
				s.mail().ListOf(w, r, who, of)
				return
			}
			s.mail().List(w, r, role)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}
	if path == "events/poll" && r.Method == "GET" {
		s.mail().PollEvents(w, r, role)
		return
	}
	if path == "events" && r.Method == "GET" {
		s.mail().Events(w, r, role)
		return
	}
	if path == "watch" && r.Method == "GET" {
		s.mail().Watch(w, r, role)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "messages" {
		id, e := wire.ParseID(parts[1])
		if e != nil {
			http.Error(w, "invalid id", 400)
			return
		}
		if len(parts) == 2 && r.Method == "GET" && s.ids().IsGlobalBoss(who) && s.mail().ViewForeign(w, who, id) {
			return
		}
		if box, other := s.mail().OtherSlotMailbox(who, id); other {
			http.Error(w, "message belongs to "+box+"; send X-Switchboard-Role: "+box+" (CLI: switchboard --role "+box+" or SWITCHBOARD_ROLE)", 403)
			return
		}
		if len(parts) == 2 && r.Method == "GET" {
			s.mail().Read(w, r, role, id)
			return
		}
		if len(parts) == 3 && r.Method == "POST" {
			if parts[2] == "reply" {
				s.mail().Reply(w, r, who, id)
				return
			}
			if parts[2] == "close" {
				s.mail().Close(w, r, role, id)
				return
			}
			if parts[2] == "ack" || parts[2] == "complete" || parts[2] == "read" {
				s.mail().Status(w, r, role, id, parts[2])
				return
			}
		}
	}
	http.NotFound(w, r)
}

// adminAPI exposes mailbox reporting and settings to a signed-in admin (see
// adminauth Identify; under the transition rule, a reverse proxy that injects the
// server-held secret). Caller-supplied identity headers are never trusted.
// Writes (PUT, POST, DELETE) also need the dashboard CSRF guard.
func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	who, ok := s.adminAuth().Identify(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		if !adminWriteAllowed(r, s.header(r, "Dashboard")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	if s.hosting && hostingAdminPath(path) {
		s.hostingAdmin(w, r, path)
		return
	}
	if path == "live" && r.Method == http.MethodGet {
		s.liveSocket(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if path == "question-refusals" {
		s.asks().QuestionRefusalsAPI(w, r)
		return
	}
	if path == "question-settings" || strings.HasPrefix(path, "question-settings/") {
		target, hasTarget := strings.CutPrefix(path, "question-settings/")
		s.asks().QuestionSettingsAPI(w, r, target, hasTarget)
		return
	}
	if rest, ok := strings.CutPrefix(path, "questions"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.asks().AdminQuestionsAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "permissions"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.asks().AdminPermissionsAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "reports"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.reports().AdminAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "jobs"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.jobs().AdminAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "schedules"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.jobs().ScheduleAdminAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "roles"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.ids().AdminRolesAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "groups"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.ids().AdminGroupsAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "invites"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.ids().AdminInvitesAPI(w, r, who.User, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "messages/"); ok && r.Method == http.MethodPost {
		// POST /api/admin/messages/{id}/close: the dashboard closes a directive.
		idText, action, _ := strings.Cut(rest, "/")
		id, err := wire.ParseID(idText)
		if err != nil || action != "close" {
			http.NotFound(w, r)
			return
		}
		s.mail().Close(w, r, "", id)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch path {
	case "me":
		// The dashboard's bootstrap call; hosting tells it whether to show
		// the terminal tab.
		// With hosting on it also lists the terminal view's assets, which
		// the page loads only then.
		var assets []string
		if s.hosting {
			assets = dashboard.TerminalAssets
		}
		writeJSON(w, struct {
			adminauth.Identity
			Hosting        bool     `json:"hosting"`
			TerminalAssets []string `json:"terminal_assets,omitempty"`
		}{who, s.hosting, assets})
	case "slots":
		out, err := s.presence().ActiveSlots("")
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
	case "presence":
		s.presence().API(w, r, nil)
	case "metrics":
		s.insight().MetricsAPI(w, r, true, true)
	case "progress":
		s.progressAPI(w, r, true)
	case "messages":
		s.mail().AdminList(w, r)
	case "decisions":
		s.asks().DecisionsAPI(w, r, nil)
	case "settings":
		s.settingsAPI(w, r)
	default:
		http.NotFound(w, r)
	}
}

// adminWriteAllowed is the CSRF guard for every non-GET admin request. The
// custom header forces a CORS preflight, which a cross-site page cannot pass.
// dashboard is the X-Switchboard-Dashboard (or old-prefix) header value.
func adminWriteAllowed(r *http.Request, dashboard string) bool {
	ct := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(ct, "application/json") || dashboard != "1" {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

// health reports liveness and the build version. It is unauthenticated; the
// version (a short commit sha in CI images, "dev" locally) is not sensitive.
// health serves GET /health: the build version and the minimum client
// version (#308), without authentication.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := wire.ServerVersionInfo{OK: true, Version: version}
	if s.presenceCfg.CLIMinOK {
		out.CLIMin = s.presenceCfg.CLIMin.String()
	}
	json.NewEncoder(w).Encode(out)
}

// configureCLIMin sets the minimum client version (#308) from
// SWITCHBOARD_CLI_MIN, else serverVersion's major.minor. An invalid setting
// does not stop the server: it logs one line and uses the default.
func configureCLIMin(s *Server, getenv func(string) string, serverVersion string, logf func(string, ...any)) {
	v, from := config.EnvFrom(getenv, config.CLIMinName)
	min, ok, bad := wire.CLIMinimum(v, serverVersion)
	if bad != "" {
		logf("%s=%q is not a version such as v0.26; using the server's own", from, bad)
	}
	s.presenceCfg.CLIMin, s.presenceCfg.CLIMinOK = min, ok
}
