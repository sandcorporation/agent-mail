package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Numbered role slots.
//
// A session authenticated with a base role (say team/worker) registers a
// machine+directory identity and receives a numbered role team/worker-N held
// by a lease. The base role's token authorises every team/worker-N. Slot 1
// shares its mailbox with the unnumbered base role: mail is stored under the
// base name, so clients that never register keep seeing the same mail as the
// session holding team/worker-1.

const (
	defaultSlotLease = 3 * time.Minute
	maxSlotNumber    = 999
	maxSlotField     = 512 // bytes, for machine, directory, session_id, label, parent
)

// principal is an authenticated caller. Role is the mailbox name used for
// storage (slot 1 is stored as its base); Base is the role whose token
// authorised the request; Number is the slot number in the header (0 when
// the header named a role row exactly).
type principal struct {
	Role   string
	Base   string
	Number int
}

// queryer is the part of *sql.DB and *sql.Tx the slot code reads through, so
// the same lookups work inside a transaction.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *Server) leaseLength() time.Duration {
	if s.slotLease > 0 {
		return s.slotLease
	}
	return defaultSlotLease
}

// splitSlot parses "<base>-N" with N a positive number without leading zeros.
func splitSlot(role string) (string, int, bool) {
	i := strings.LastIndexByte(role, '-')
	if i <= 0 || i == len(role)-1 {
		return "", 0, false
	}
	digits := role[i+1:]
	if digits[0] == '0' {
		return "", 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > maxSlotNumber {
		return "", 0, false
	}
	return role[:i], n, true
}

func slotRole(base string, n int) string { return base + "-" + strconv.Itoa(n) }

func roleToken(q queryer, role string) (string, bool) {
	var token string
	if q.QueryRow(`SELECT token FROM roles WHERE role=?`, role).Scan(&token) != nil {
		return "", false
	}
	return token, true
}

func roleExists(q queryer, role string) bool { _, ok := roleToken(q, role); return ok }

func sameSecret(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// authenticate checks the bearer token against X-Agent-Mail-Role. A role row
// with exactly that name always wins, so a pre-existing role ending in -N
// keeps its own token. Otherwise "<base>-N" is accepted with the token of
// <base>. The slot need not be currently leased: the base token can claim any
// slot anyway, and mail queued for an expired slot must stay readable.
func (s *Server) authenticate(r *http.Request) (principal, bool) {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h := r.Header.Get("X-Agent-Mail-Role")
	if t == "" || h == "" || !validRole(h) {
		return principal{}, false
	}
	if token, ok := roleToken(s.db, h); ok {
		if !sameSecret(token, t) {
			return principal{}, false
		}
		return principal{Role: h, Base: h}, true
	}
	base, n, ok := splitSlot(h)
	if !ok {
		return principal{}, false
	}
	token, ok := roleToken(s.db, base)
	if !ok || !sameSecret(token, t) {
		return principal{}, false
	}
	p := principal{Role: h, Base: base, Number: n}
	if n == 1 {
		p.Role = base
	}
	return p, true
}

// mailbox returns the stored name for an address: an exact role row as is,
// "<base>-1" as <base>, "<base>-N" as itself. ok is false when no role row
// backs the address.
func mailbox(q queryer, name string) (string, bool) {
	if roleExists(q, name) {
		return name, true
	}
	base, n, ok := splitSlot(name)
	if !ok || !roleExists(q, base) {
		return "", false
	}
	if n == 1 {
		return base, true
	}
	return name, true
}

// slotRequest is the body of the slot endpoints. Machine and Directory form
// the identity; the rest is stored for display (and for presence and label
// routing later).
type slotRequest struct {
	Machine   string `json:"machine"`
	Directory string `json:"directory"`
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
	Parent    string `json:"parent"`
}

func (q slotRequest) valid() bool {
	if q.Machine == "" || q.Directory == "" {
		return false
	}
	for _, f := range []string{q.Machine, q.Directory, q.SessionID, q.Label, q.Parent} {
		if len(f) > maxSlotField {
			return false
		}
	}
	return true
}

// slotView is one row of the slot table.
type slotView struct {
	Role           string    `json:"role"`
	Base           string    `json:"base"`
	Number         int       `json:"number"`
	Machine        string    `json:"machine"`
	Directory      string    `json:"directory"`
	SessionID      string    `json:"session_id,omitempty"`
	Label          string    `json:"label,omitempty"`
	Parent         string    `json:"parent,omitempty"`
	RegisteredAt   time.Time `json:"registered_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// slotGrant answers register, heartbeat and takeover.
type slotGrant struct {
	slotView
	LeaseSeconds int       `json:"lease_seconds"`
	OtherActive  int       `json:"other_active"`        // other leased slots under the same base
	Reclaimed    bool      `json:"reclaimed"`           // the identity already held this number
	Displaced    *slotView `json:"displaced,omitempty"` // takeover: where the previous -1 holder went
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func slotCols() string {
	return `base,number,machine,directory,session_id,label,parent,registered_at,lease_until`
}
func scanSlot(sc interface{ Scan(...any) error }) (slotView, error) {
	var v slotView
	var reg, until int64
	err := sc.Scan(&v.Base, &v.Number, &v.Machine, &v.Directory, &v.SessionID, &v.Label, &v.Parent, &reg, &until)
	v.Role = slotRole(v.Base, v.Number)
	v.RegisteredAt, v.LeaseExpiresAt = fromMS(reg), fromMS(until)
	return v, err
}

// slotAPI serves /api/slots and /api/slots/{register,heartbeat,takeover,release}.
func (s *Server) slotAPI(w http.ResponseWriter, r *http.Request, p principal, action string) {
	if action == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		out, err := s.activeSlots(p.Base)
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req slotRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req) != nil || !req.valid() {
		http.Error(w, "machine and directory required (each field at most 512 bytes)", 400)
		return
	}
	var (
		g   slotGrant
		err error
	)
	switch action {
	case "register":
		g, err = s.claimSlot(r.Context(), p.Base, req, false)
	case "takeover":
		g, err = s.claimSlot(r.Context(), p.Base, req, true)
	case "heartbeat":
		g, err = s.renewSlot(r.Context(), p.Base, req, false)
	case "release":
		g, err = s.renewSlot(r.Context(), p.Base, req, true)
	default:
		http.NotFound(w, r)
		return
	}
	switch {
	case errors.Is(err, errNoSlot):
		http.Error(w, "no slot for this machine and directory; register again", 404)
	case errors.Is(err, errSlotsFull):
		http.Error(w, "no free slot", 409)
	case errors.Is(err, errSlotReserved):
		http.Error(w, "slot 1 of this base is a separately registered role", 409)
	case err != nil:
		http.Error(w, "slot update failed", 500)
	default:
		writeJSON(w, g)
	}
}

var (
	errNoSlot       = errors.New("no slot")
	errSlotsFull    = errors.New("no free slot")
	errSlotReserved = errors.New("slot reserved")
)

func (s *Server) activeSlots(base string) ([]slotView, error) {
	query := `SELECT ` + slotCols() + ` FROM slots WHERE lease_until>?`
	args := []any{ms(s.now())}
	if base != "" {
		query += ` AND base=?`
		args = append(args, base)
	}
	rows, err := s.db.Query(query+` ORDER BY base,number`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []slotView{}
	for rows.Next() {
		v, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// inTx runs f in one transaction. The pool has a single connection, so slot
// changes are serialised; the (base, number) primary key and the
// (base, machine, directory) unique key back that up.
func (s *Server) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// freeNumber returns the smallest number under base that is not leased, not
// a separately registered role and not in skip. An expired row on that number
// is removed so the caller can take it.
func freeNumber(tx *sql.Tx, base string, now int64, skip map[int]bool) (int, error) {
	leased := map[int]bool{}
	rows, err := tx.Query(`SELECT number FROM slots WHERE base=? AND lease_until>?`, base, now)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var n int
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		leased[n] = true
	}
	rows.Close()
	for n := 1; n <= maxSlotNumber; n++ {
		if leased[n] || skip[n] || roleExists(tx, slotRole(base, n)) {
			continue
		}
		if _, err = tx.Exec(`DELETE FROM slots WHERE base=? AND number=?`, base, n); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, errSlotsFull
}

func identityRow(tx *sql.Tx, base string, req slotRequest) (slotView, bool, error) {
	v, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND machine=? AND directory=?`, base, req.Machine, req.Directory))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// claimSlot registers (takeover=false) or takes slot 1 (takeover=true).
//
// Register: an identity that already has a row gets that number back, leased
// or not (a row only disappears when another identity takes its number after
// expiry). A new identity gets the smallest free number.
//
// Takeover: the caller gets number 1. A leased previous holder moves to the
// smallest free number other than 1, keeping its lease; an expired one is
// dropped. The caller's own previous number, if any, is released.
func (s *Server) claimSlot(ctx context.Context, base string, req slotRequest, takeover bool) (slotGrant, error) {
	now := s.now()
	until := now.Add(s.leaseLength())
	var g slotGrant
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		own, has, err := identityRow(tx, base, req)
		if err != nil {
			return err
		}
		number, registered := own.Number, now
		if has {
			registered = own.RegisteredAt
		}
		switch {
		case takeover:
			if roleExists(tx, slotRole(base, 1)) {
				return errSlotReserved
			}
			g.Reclaimed = has && own.Number == 1
			if has && own.Number != 1 {
				if _, err = tx.Exec(`DELETE FROM slots WHERE base=? AND number=?`, base, own.Number); err != nil {
					return err
				}
				registered = now
			}
			if !g.Reclaimed {
				if g.Displaced, err = displaceFirst(tx, base, ms(now)); err != nil {
					return err
				}
			}
			number = 1
		case has:
			g.Reclaimed = true
		default:
			if number, err = freeNumber(tx, base, ms(now), nil); err != nil {
				return err
			}
		}
		if g.Reclaimed {
			_, err = tx.Exec(`UPDATE slots SET session_id=?,label=?,parent=?,lease_until=? WHERE base=? AND number=?`, req.SessionID, req.Label, req.Parent, ms(until), base, number)
		} else {
			_, err = tx.Exec(`INSERT INTO slots(`+slotCols()+`) VALUES(?,?,?,?,?,?,?,?,?)`, base, number, req.Machine, req.Directory, req.SessionID, req.Label, req.Parent, ms(registered), ms(until))
		}
		if err != nil {
			return err
		}
		return s.fillGrant(tx, &g, base, number)
	})
	return g, err
}

// displaceFirst moves the current holder of number 1 out of the way.
func displaceFirst(tx *sql.Tx, base string, now int64) (*slotView, error) {
	cur, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND number=1`, base))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM slots WHERE base=? AND number=1`, base); err != nil {
		return nil, err
	}
	if ms(cur.LeaseExpiresAt) <= now {
		return nil, nil
	}
	n, err := freeNumber(tx, base, now, map[int]bool{1: true})
	if errors.Is(err, errSlotsFull) {
		return nil, nil // nowhere to go: the old holder is released
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO slots(`+slotCols()+`) VALUES(?,?,?,?,?,?,?,?,?)`, base, n, cur.Machine, cur.Directory, cur.SessionID, cur.Label, cur.Parent, ms(cur.RegisteredAt), ms(cur.LeaseExpiresAt)); err != nil {
		return nil, err
	}
	cur.Number, cur.Role = n, slotRole(base, n)
	return &cur, nil
}

// renewSlot extends (or, with release, ends) the lease of the identity's slot.
// An expired slot whose number nobody took is renewed as well. The answer
// carries the identity's current role, which differs from the caller's when a
// takeover moved it.
func (s *Server) renewSlot(ctx context.Context, base string, req slotRequest, release bool) (slotGrant, error) {
	now := s.now()
	until := now.Add(s.leaseLength())
	if release {
		until = now
	}
	var g slotGrant
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		own, has, err := identityRow(tx, base, req)
		if err != nil {
			return err
		}
		if !has {
			return errNoSlot
		}
		set, args := `lease_until=?`, []any{ms(until)}
		if !release {
			set += `,session_id=?,label=?,parent=?`
			args = append(args, req.SessionID, req.Label, req.Parent)
		}
		if _, err = tx.Exec(`UPDATE slots SET `+set+` WHERE base=? AND number=?`, append(args, base, own.Number)...); err != nil {
			return err
		}
		g.Reclaimed = true
		return s.fillGrant(tx, &g, base, own.Number)
	})
	return g, err
}

func (s *Server) fillGrant(tx *sql.Tx, g *slotGrant, base string, number int) error {
	v, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND number=?`, base, number))
	if err != nil {
		return err
	}
	g.slotView = v
	g.LeaseSeconds = int(s.leaseLength() / time.Second)
	return tx.QueryRow(`SELECT COUNT(*) FROM slots WHERE base=? AND number<>? AND lease_until>?`, base, number, ms(s.now())).Scan(&g.OtherActive)
}
