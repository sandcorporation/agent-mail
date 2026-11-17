package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Permission requests.
//
// A session asks for permission to use a tool (POST /api/permissions); the
// server gives the request an opaque ID and an expiry
// (AGENT_MAIL_PERMISSION_TTL, default 10m). Exactly one allow or deny answer
// is accepted, from:
//
//   - the requester's parent: the parent reported on the requester's active
//     slot, read once when the request is made and fixed on the request, so
//     changing the slot afterwards cannot redirect it; or
//   - a boss role listed in AGENT_MAIL_BOSS_ROLES.
//
// The parent is self-reported by the requester at registration, so it is
// only trusted when it belongs to a different base: every holder of a base
// token can act as any <base>-N, so a same-base parent (the requester itself
// or a sibling slot) is no independent check and is ignored. For the same
// reason the requester's base never answers its own request, even when it is
// a boss. Authority is per base: any <base>-N of the parent or boss base may
// answer, since one token covers them all.
//
// The answerers are told by urgent mail from the system sender (the parent,
// or every boss when there is no trusted parent); the requester gets urgent
// mail with the answer and can also read GET /api/permissions/{id}.

const (
	defaultPermissionTTL = 10 * time.Minute
	maxPermissionTool    = 128 // bytes
	maxPermissionText    = 500 // bytes, for summary and note
	permissionIDPrefix   = "perm_"
)

func (s *Server) permissionTTLValue() time.Duration {
	if s.permissionTTL > 0 {
		return s.permissionTTL
	}
	return defaultPermissionTTL
}

func migratePermissions(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS permissions(id TEXT PRIMARY KEY,requester TEXT NOT NULL,requester_base TEXT NOT NULL,tool TEXT NOT NULL,summary TEXT NOT NULL,declared_parent TEXT NOT NULL DEFAULT '',parent TEXT NOT NULL DEFAULT '',parent_base TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,decision TEXT NOT NULL DEFAULT '',note TEXT NOT NULL DEFAULT '',answered_by TEXT NOT NULL DEFAULT '',answered_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_permissions_requester ON permissions(requester_base,created_at);
CREATE INDEX IF NOT EXISTS idx_permissions_parent ON permissions(parent_base,created_at);`)
	return err
}

// permissionView is the JSON of one request.
type permissionView struct {
	ID            string     `json:"id"`
	Requester     string     `json:"requester"`
	Tool          string     `json:"tool"`
	Summary       string     `json:"summary"`
	Parent        string     `json:"parent,omitempty"`         // trusted parent fixed at request time
	IgnoredParent string     `json:"ignored_parent,omitempty"` // reported parent that was not trusted
	Status        string     `json:"status"`                   // pending | allowed | denied | expired
	Decision      string     `json:"decision,omitempty"`       // allow | deny
	Note          string     `json:"note,omitempty"`
	AnsweredBy    string     `json:"answered_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	AnsweredAt    *time.Time `json:"answered_at,omitempty"`
	CanAnswer     bool       `json:"can_answer"` // the caller may answer it now

	requesterBase, parentBase string
}

type permissionList struct {
	Permissions []permissionView `json:"permissions"`
}

func permissionCols() string {
	return `id,requester,requester_base,tool,summary,declared_parent,parent,parent_base,created_at,expires_at,decision,note,answered_by,answered_at`
}

func scanPermission(sc interface{ Scan(...any) error }, now time.Time) (permissionView, error) {
	var v permissionView
	var declared string
	var created, expires, answered int64
	err := sc.Scan(&v.ID, &v.Requester, &v.requesterBase, &v.Tool, &v.Summary, &declared, &v.Parent, &v.parentBase, &created, &expires, &v.Decision, &v.Note, &v.AnsweredBy, &answered)
	if declared != "" && v.Parent == "" {
		v.IgnoredParent = declared
	}
	v.CreatedAt, v.ExpiresAt = fromMS(created), fromMS(expires)
	if answered > 0 {
		t := fromMS(answered)
		v.AnsweredAt = &t
	}
	switch {
	case v.Decision == "allow":
		v.Status = "allowed"
	case v.Decision == "deny":
		v.Status = "denied"
	case expires <= ms(now):
		v.Status = "expired"
	default:
		v.Status = "pending"
	}
	return v, err
}

// baseOf is the base whose token authorises name: an exact role row, else
// the base of <base>-N. It is "" for unknown or reserved names.
func baseOf(q queryer, name string) string {
	if name == "" || reservedRole(name) {
		return ""
	}
	if roleExists(q, name) {
		return name
	}
	if base, _, ok := splitSlot(name); ok && roleExists(q, base) {
		return base
	}
	return ""
}

func (s *Server) isBoss(p principal) bool { return s.bossRoles[p.Base] || s.bossRoles[p.Role] }

// mayAnswer: the parent base or a boss, never the requester's own base.
func (s *Server) mayAnswer(p principal, v permissionView) bool {
	if p.Base == v.requesterBase {
		return false
	}
	return (v.parentBase != "" && p.Base == v.parentBase) || s.isBoss(p)
}

func (s *Server) mayRead(p principal, v permissionView) bool {
	return p.Base == v.requesterBase || s.mayAnswer(p, v)
}

// permissionAPI serves /api/permissions and /api/permissions/{id}[/answer].
func (s *Server) permissionAPI(w http.ResponseWriter, r *http.Request, p principal, rest string) {
	parts := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		s.requestPermission(w, r, p)
	case rest == "" && r.Method == http.MethodGet:
		s.listPermissions(w, r, p)
	case len(parts) == 1 && r.Method == http.MethodGet:
		v, code := s.loadPermission(p, parts[0])
		if code != 0 {
			permissionError(w, code)
			return
		}
		writeJSON(w, v)
	case len(parts) == 2 && parts[1] == "answer" && r.Method == http.MethodPost:
		s.answerPermission(w, r, p, parts[0])
	case rest == "" || len(parts) <= 2:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

var permissionErrors = map[int]string{
	http.StatusNotFound:  "unknown permission request",
	http.StatusForbidden: "not allowed to answer or read this permission request",
	http.StatusConflict:  "permission request already answered",
	http.StatusGone:      "permission request expired",
}

func permissionError(w http.ResponseWriter, code int) { http.Error(w, permissionErrors[code], code) }

func newPermissionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return permissionIDPrefix + hex.EncodeToString(b), nil
}

func (s *Server) requestPermission(w http.ResponseWriter, r *http.Request, p principal) {
	var req struct {
		Tool    string `json:"tool"`
		Summary string `json:"summary"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Tool == "" || len(req.Tool) > maxPermissionTool || req.Summary == "" || len(req.Summary) > maxPermissionText {
		http.Error(w, fmt.Sprintf("tool (1-%d bytes) and summary (1-%d bytes) required", maxPermissionTool, maxPermissionText), 400)
		return
	}
	id, err := newPermissionID()
	if err != nil {
		http.Error(w, "could not create request", 500)
		return
	}
	now := s.now()
	expires := now.Add(s.permissionTTLValue())
	var v permissionView
	var notify []string
	errNobody := errors.New("nobody")
	err = s.inTx(r.Context(), func(tx *sql.Tx) error {
		// The parent comes from the requester's active slot, read now and
		// fixed on the request.
		number := p.Number
		if number == 0 {
			number = 1
		}
		var declared string
		if e := tx.QueryRow(`SELECT parent FROM slots WHERE base=? AND number=? AND lease_until>?`, p.Base, number, ms(now)).Scan(&declared); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		parent, parentBase := "", ""
		if pb := baseOf(tx, declared); pb != "" && pb != p.Base {
			parent, _ = mailbox(tx, declared)
			parentBase = pb
		}
		switch {
		case parent != "":
			notify = []string{parent}
		default:
			// Registered bosses of another base; the requester's own base
			// cannot answer.
			for _, boss := range sortedKeys(s.bossRoles) {
				if b := baseOf(tx, boss); b != "" && b != p.Base {
					box, _ := mailbox(tx, boss)
					notify = append(notify, box)
				}
			}
			if len(notify) == 0 {
				return errNobody
			}
		}
		if _, e := tx.Exec(`INSERT INTO permissions(id,requester,requester_base,tool,summary,declared_parent,parent,parent_base,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, p.Role, p.Base, req.Tool, req.Summary, declared, parent, parentBase, ms(now), ms(expires)); e != nil {
			return e
		}
		subject := "permission: " + req.Tool + " for " + p.Role
		body := fmt.Sprintf("%s asks to use %s: %s\nAnswer before %s: agent-mail permission answer %s --decision allow|deny",
			p.Role, req.Tool, req.Summary, expires.UTC().Format(time.RFC3339), id)
		for _, box := range notify {
			if e := insertSystemMail(tx, box, subject, body, id, "urgent", "", now); e != nil {
				return e
			}
		}
		var e error
		v, e = scanPermission(tx.QueryRow(`SELECT `+permissionCols()+` FROM permissions WHERE id=?`, id), now)
		return e
	})
	if errors.Is(err, errNobody) {
		http.Error(w, "nobody can answer: no parent of another base on an active slot and no boss role configured", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "could not store request", 500)
		return
	}
	for _, box := range notify {
		s.hub.publish(box)
	}
	w.WriteHeader(201)
	writeJSON(w, v)
}

// loadPermission returns the request when p may read it, else an HTTP code.
func (s *Server) loadPermission(p principal, id string) (permissionView, int) {
	if !strings.HasPrefix(id, permissionIDPrefix) || len(id) > 64 {
		return permissionView{}, http.StatusNotFound
	}
	v, err := scanPermission(s.db.QueryRow(`SELECT `+permissionCols()+` FROM permissions WHERE id=?`, id), s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return v, http.StatusNotFound
	}
	if err != nil {
		return v, http.StatusInternalServerError
	}
	if !s.mayRead(p, v) {
		return permissionView{}, http.StatusForbidden
	}
	v.CanAnswer = v.Status == "pending" && s.mayAnswer(p, v)
	return v, 0
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request, p principal) {
	pendingOnly := r.URL.Query().Get("pending") == "1"
	now := s.now()
	q := `SELECT ` + permissionCols() + ` FROM permissions WHERE requester_base=? OR (parent_base=? AND requester_base<>?)`
	args := []any{p.Base, p.Base, p.Base}
	if s.isBoss(p) {
		q = `SELECT ` + permissionCols() + ` FROM permissions WHERE 1=1`
		args = nil
	}
	if pendingOnly {
		q = `SELECT * FROM (` + q + `) WHERE decision='' AND expires_at>?`
		args = append(args, ms(now))
	}
	rows, err := s.db.Query(q+` ORDER BY created_at DESC LIMIT 100`, args...)
	if err != nil {
		http.Error(w, "could not list requests", 500)
		return
	}
	defer rows.Close()
	out := permissionList{Permissions: []permissionView{}}
	for rows.Next() {
		v, err := scanPermission(rows, now)
		if err != nil {
			http.Error(w, "could not list requests", 500)
			return
		}
		if !s.mayRead(p, v) {
			continue
		}
		v.CanAnswer = v.Status == "pending" && s.mayAnswer(p, v)
		out.Permissions = append(out.Permissions, v)
	}
	writeJSON(w, out)
}

func (s *Server) answerPermission(w http.ResponseWriter, r *http.Request, p principal, id string) {
	var req struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Decision != "allow" && req.Decision != "deny" {
		http.Error(w, `decision must be "allow" or "deny"`, 400)
		return
	}
	if len(req.Note) > maxPermissionText {
		http.Error(w, fmt.Sprintf("note must be at most %d bytes", maxPermissionText), 400)
		return
	}
	v, code := s.loadPermission(p, id)
	if code == 0 && !s.mayAnswer(p, v) {
		code = http.StatusForbidden
	}
	if code != 0 {
		permissionError(w, code)
		return
	}
	now := s.now()
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE permissions SET decision=?,note=?,answered_by=?,answered_at=? WHERE id=? AND decision='' AND expires_at>?`,
			req.Decision, req.Note, p.Role, ms(now), id, ms(now))
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			var decision string
			if e = tx.QueryRow(`SELECT decision FROM permissions WHERE id=?`, id).Scan(&decision); e != nil {
				return e
			}
			if decision != "" {
				return errAnswered
			}
			return errExpired
		}
		if v, e = scanPermission(tx.QueryRow(`SELECT `+permissionCols()+` FROM permissions WHERE id=?`, id), now); e != nil {
			return e
		}
		word := map[string]string{"allow": "allowed", "deny": "denied"}[req.Decision]
		body := fmt.Sprintf("Request %s (%s) was %s by %s.", id, v.Tool, word, p.Role)
		if req.Note != "" {
			body += "\nNote: " + req.Note
		}
		if box, ok := mailbox(tx, v.Requester); ok {
			return insertSystemMail(tx, box, "permission "+word+": "+v.Tool, body, id, "urgent", "", now)
		}
		return nil
	})
	switch {
	case errors.Is(err, errAnswered):
		permissionError(w, http.StatusConflict)
		return
	case errors.Is(err, errExpired):
		permissionError(w, http.StatusGone)
		return
	case err != nil:
		http.Error(w, "could not store answer", 500)
		return
	}
	if box, ok := mailbox(s.db, v.Requester); ok {
		s.hub.publish(box)
	}
	writeJSON(w, v)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	errAnswered = errors.New("already answered")
	errExpired  = errors.New("expired")
)
