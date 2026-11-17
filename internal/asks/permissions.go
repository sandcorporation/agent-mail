package asks

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/mail"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Permission requests.
//
// A session asks for permission to use a tool (POST /api/permissions); the
// server gives the request an opaque ID and an expiry
// (AGENT_MAIL_PERMISSION_TTL, default 10m). Exactly one allow or deny answer
// is accepted, and only from a boss role listed in AGENT_MAIL_BOSS_ROLES
// (matched by base or role). Sessions on one machine can read each other's
// profile files, so any other role, the requester's parent included, is no
// independent check; the boss credential lives on a separate machine. The
// requester's own base never answers its own request, even when it is a
// boss: every holder of a base token can act as any <base>-N. Authority is
// per base: any <base>-N of a boss base may answer, since one token covers
// them all.
//
// The parent reported on the requester's active slot is read once when the
// request is made and fixed on the request, so changing the slot afterwards
// cannot redirect it. It is only trusted when it belongs to a different
// base (a same-base parent is the requester itself or a sibling slot). A
// trusted parent may read the request and is told about it, but cannot
// answer.
//
// Every boss of another base gets urgent mail from the system sender with
// the answer command; a trusted parent also gets urgent mail saying a boss
// must answer, without the command. With no boss of another base the
// request is refused (409) and the session falls back to its local prompt.
// The requester gets urgent mail with the answer and can also read
// GET /api/permissions/{id}.

const (
	defaultPermissionTTL = 10 * time.Minute
	maxPermissionTool    = 128 // bytes
	maxPermissionText    = 500 // bytes, for summary and note
	permissionIDPrefix   = "perm_"
)

// PermissionTTL is how long a permission request stays open.
func (s *Service) PermissionTTL() time.Duration {
	if s.cfg.PermissionTTL > 0 {
		return s.cfg.PermissionTTL
	}
	return defaultPermissionTTL
}

func permissionCols() string {
	return `id,requester,requester_base,tool,summary,declared_parent,parent,parent_base,created_at,expires_at,decision,note,answered_by,answered_at`
}

func scanPermission(sc interface{ Scan(...any) error }, now time.Time) (wire.PermissionView, error) {
	var v wire.PermissionView
	var declared string
	var created, expires, answered int64
	err := sc.Scan(&v.ID, &v.Requester, &v.RequesterBase, &v.Tool, &v.Summary, &declared, &v.Parent, &v.ParentBase, &created, &expires, &v.Decision, &v.Note, &v.AnsweredBy, &answered)
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

// mayAnswer: the requester's boss (identity BossesFor), never its own base.
func (s *Service) mayAnswer(p identity.Principal, v wire.PermissionView) bool {
	return p.Base != v.RequesterBase && s.ids.IsBossFor(p, v.RequesterBase)
}

// mayRead: the requester's base, the recorded parent base, or a boss.
func (s *Service) mayRead(p identity.Principal, v wire.PermissionView) bool {
	return p.Base == v.RequesterBase || (v.ParentBase != "" && p.Base == v.ParentBase) || s.mayAnswer(p, v)
}

// PermissionAPI serves /api/permissions and /api/permissions/{id}[/answer].
func (s *Service) PermissionAPI(w http.ResponseWriter, r *http.Request, p identity.Principal, rest string) {
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

func (s *Service) requestPermission(w http.ResponseWriter, r *http.Request, p identity.Principal) {
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
	expires := now.Add(s.PermissionTTL())
	var v wire.PermissionView
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
		if pb := identity.BaseOf(tx, declared); pb != "" && pb != p.Base && s.ids.MaySee(p, pb) {
			parent, _ = identity.Mailbox(tx, declared)
			parentBase = pb
		}
		// The requester's registered bosses of another base answer; the
		// requester's own base cannot.
		for _, boss := range s.ids.BossesFor(p.Base) {
			if b := identity.BaseOf(tx, boss); b != "" && b != p.Base {
				box, _ := identity.Mailbox(tx, boss)
				notify = append(notify, box)
			}
		}
		if len(notify) == 0 {
			return errNobody
		}
		// A trusted parent is told, but cannot answer unless it is a boss.
		var inform []string
		if parent != "" && !slices.Contains(notify, parent) {
			if s.ids.IsBossFor(identity.Principal{Role: parent, Base: parentBase}, p.Base) {
				notify = append(notify, parent)
			} else {
				inform = []string{parent}
			}
		}
		if _, e := tx.Exec(`INSERT INTO permissions(id,requester,requester_base,tool,summary,declared_parent,parent,parent_base,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, p.Role, p.Base, req.Tool, req.Summary, declared, parent, parentBase, ms(now), ms(expires)); e != nil {
			return e
		}
		subject := "permission: " + req.Tool + " for " + p.Role
		deadline := expires.UTC().Format(time.RFC3339)
		body := fmt.Sprintf("%s asks to use %s: %s\nAnswer before %s: agent-mail permission answer %s --decision allow|deny",
			p.Role, req.Tool, req.Summary, deadline, id)
		info := fmt.Sprintf("%s asks to use %s: %s\nRequest %s expires at %s. A boss role must answer it; you are told as the parent and cannot answer.",
			p.Role, req.Tool, req.Summary, id, deadline)
		for _, box := range notify {
			if e := mail.InsertSystemMail(tx, box, subject, body, id, "urgent", "", now); e != nil {
				return e
			}
		}
		for _, box := range inform {
			if e := mail.InsertSystemMail(tx, box, subject, info, id, "urgent", "", now); e != nil {
				return e
			}
		}
		notify = append(notify, inform...)
		var e error
		v, e = scanPermission(tx.QueryRow(`SELECT `+permissionCols()+` FROM permissions WHERE id=?`, id), now)
		return e
	})
	if errors.Is(err, errNobody) {
		http.Error(w, "nobody can answer: no boss role of another base configured", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "could not store request", 500)
		return
	}
	for _, box := range notify {
		s.hub.Publish(box)
	}
	w.WriteHeader(201)
	writeJSON(w, v)
}

// loadPermission returns the request when p may read it, else an HTTP code.
func (s *Service) loadPermission(p identity.Principal, id string) (wire.PermissionView, int) {
	if !strings.HasPrefix(id, permissionIDPrefix) || len(id) > 64 {
		return wire.PermissionView{}, http.StatusNotFound
	}
	v, err := scanPermission(s.db.QueryRow(`SELECT `+permissionCols()+` FROM permissions WHERE id=?`, id), s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return v, http.StatusNotFound
	}
	if err != nil {
		return v, http.StatusInternalServerError
	}
	if !s.mayRead(p, v) {
		return wire.PermissionView{}, http.StatusForbidden
	}
	v.CanAnswer = v.Status == "pending" && s.mayAnswer(p, v)
	return v, 0
}

func (s *Service) listPermissions(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	pendingOnly := r.URL.Query().Get("pending") == "1"
	now := s.now()
	q := `SELECT ` + permissionCols() + ` FROM permissions WHERE requester_base=? OR (parent_base=? AND requester_base<>?)`
	args := []any{p.Base, p.Base, p.Base}
	if cond, more := s.ids.AnswerableSQL(p, "permissions.requester_base"); cond != "" {
		q += ` OR ` + cond
		args = append(args, more...)
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
	out := wire.PermissionList{Permissions: []wire.PermissionView{}}
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

func (s *Service) answerPermission(w http.ResponseWriter, r *http.Request, p identity.Principal, id string) {
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
		if box, ok := identity.Mailbox(tx, v.Requester); ok {
			return mail.InsertSystemMail(tx, box, "permission "+word+": "+v.Tool, body, id, "urgent", "", now)
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
	if box, ok := identity.Mailbox(s.db, v.Requester); ok {
		s.hub.Publish(box)
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
