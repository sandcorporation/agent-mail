package presence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
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
	maxSlotNumber    = wire.MaxSlotNumber
	maxSlotField     = 512 // bytes, for machine, directory, session_id, label, parent
)

// LeaseLength is SWITCHBOARD_SLOT_LEASE when set, otherwise the presence
// heartbeat interval times the number of heartbeats a session may miss
// (60s × 3 = defaultSlotLease). Offline means the lease has ended.
func (s *Service) LeaseLength() time.Duration {
	if s.cfg.SlotLease > 0 {
		return s.cfg.SlotLease
	}
	return s.heartbeatInterval() * time.Duration(s.missedHeartbeats())
}

func slotCols() string {
	return `base,number,machine,directory,session_id,label,parent,registered_at,lease_until`
}
func scanSlot(sc interface{ Scan(...any) error }) (wire.SlotView, error) {
	var v wire.SlotView
	var reg, until int64
	err := sc.Scan(&v.Base, &v.Number, &v.Machine, &v.Directory, &v.SessionID, &v.Label, &v.Parent, &reg, &until)
	v.Role = identity.SlotRole(v.Base, v.Number)
	v.RegisteredAt, v.LeaseExpiresAt = fromMS(reg), fromMS(until)
	return v, err
}

// SlotAPI serves /api/slots and /api/slots/{register,heartbeat,takeover,release}.
func (s *Service) SlotAPI(w http.ResponseWriter, r *http.Request, p identity.Principal, action string) {
	if action == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		out, err := s.ActiveSlots(p.Base)
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
	var req wire.SlotRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req) != nil {
		http.Error(w, "invalid JSON body", 400)
		return
	}
	req.CLIVersion = cliVersionHeader(r)
	if !slotRequestValid(req) {
		http.Error(w, "machine and directory required (each field at most 512 bytes)", 400)
		return
	}
	if _, err := req.Presence(); err != nil && action != "release" {
		http.Error(w, err.Error(), 400)
		return
	}
	var (
		g   wire.SlotGrant
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

// ActiveSlots lists the leased slots, of base only when base is not empty.
func (s *Service) ActiveSlots(base string) ([]wire.SlotView, error) {
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
	out := []wire.SlotView{}
	for rows.Next() {
		v, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FolderSlot is the slot of base held at machine in folder: the identity
// (machine, folder) itself or a session-marked form "<folder> #<8 hex>" of
// it (mail.Folder). Of several, a leased one wins over an ended one, then
// the one heard from last. It only reads; ok is false when there is none.
func (s *Service) FolderSlot(base, machine, folder string) (v wire.SlotView, ok bool, err error) {
	rows, err := s.db.Query(`SELECT `+slotCols()+` FROM slots WHERE base=? AND machine=? AND substr(directory,1,length(?))=?`, base, machine, folder, folder)
	if err != nil {
		return v, false, err
	}
	defer rows.Close()
	now := s.now()
	for rows.Next() {
		c, err := scanSlot(rows)
		if err != nil {
			return v, false, err
		}
		if mail.Folder(c.Directory) != folder {
			continue
		}
		cLive, vLive := c.LeaseExpiresAt.After(now), v.LeaseExpiresAt.After(now)
		if !ok || cLive && !vLive || cLive == vLive && c.LeaseExpiresAt.After(v.LeaseExpiresAt) {
			v, ok = c, true
		}
	}
	return v, ok, rows.Err()
}

// inTx runs f in one transaction. The pool has a single connection, so slot
// changes are serialised; the (base, number) primary key and the
// (base, machine, directory) unique key back that up.
func (s *Service) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	return store.InTx(ctx, s.db, f)
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
		if leased[n] || skip[n] || identity.RoleExists(tx, identity.SlotRole(base, n)) {
			continue
		}
		if err = LogOffline(tx, base, n, fromMS(now)); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`DELETE FROM slots WHERE base=? AND number=?`, base, n); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, errSlotsFull
}

func identityRow(tx *sql.Tx, base string, req wire.SlotRequest) (wire.SlotView, bool, error) {
	v, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND machine=? AND directory=?`, base, req.Machine, req.Directory))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// endLeases hands off what the slots of base whose lease has ended still
// hold unread for another session (mail.HandOffUnread), except the slot of
// req's own identity, which renews its lease in this transaction. Every
// register and heartbeat runs it first, so it sees an ended lease before the
// number is given to another identity and before held mail is delivered; a
// handoff can only be taken in such a transaction. It returns the mailboxes
// of senders told that their mail was held (#354), to wake after commit.
func endLeases(tx *sql.Tx, base string, req wire.SlotRequest, now time.Time) ([]string, error) {
	rows, err := tx.Query(`SELECT number FROM slots WHERE base=? AND lease_until<=? AND NOT (machine=? AND directory=?)`, base, ms(now), req.Machine, req.Directory)
	if err != nil {
		return nil, err
	}
	var ended []int
	for rows.Next() {
		var n int
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		ended = append(ended, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var told []string
	for _, n := range ended {
		_, t, err := mail.HandOffUnread(tx, base, n, now)
		if err != nil {
			return nil, err
		}
		told = append(told, t...)
	}
	return told, nil
}

// wakeTold wakes the senders endLeases told, once the transaction committed.
func (s *Service) wakeTold(err error, told []string) {
	if err != nil {
		return
	}
	for _, box := range told {
		s.wake(box)
	}
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
func (s *Service) claimSlot(ctx context.Context, base string, req wire.SlotRequest, takeover bool) (wire.SlotGrant, error) {
	now := s.now()
	until := now.Add(s.LeaseLength())
	var g wire.SlotGrant
	var box, handed string
	var moved int
	var told []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if told, err = endLeases(tx, base, req, now); err != nil {
			return err
		}
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
			if identity.RoleExists(tx, identity.SlotRole(base, 1)) {
				return errSlotReserved
			}
			g.Reclaimed = has && own.Number == 1
			if has && own.Number != 1 {
				if err = LogOffline(tx, base, own.Number, now); err != nil {
					return err
				}
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
		if err = applyPresence(tx, base, number, req, now, true); err != nil {
			return err
		}
		if box, g.HeldDelivered, err = mail.DeliverHeld(tx, base, number, req.Label, now); err != nil {
			return err
		}
		if handed, moved, err = mail.DeliverHandoff(tx, base, number, req.Machine, req.Directory, req.Label, now); err != nil {
			return err
		}
		return s.fillGrant(tx, &g, base, number)
	})
	s.wakeHeld(err, box, g.HeldDelivered)
	s.wakeHeld(err, handed, moved)
	s.wakeTold(err, told)
	return g, err
}

// wakeHeld tells subscribers of box about held mail a committed slot change
// moved.
func (s *Service) wakeHeld(err error, box string, moved int) {
	if err == nil && moved > 0 {
		s.wake(box)
	}
}

// displaceFirst moves the current holder of number 1 out of the way.
func displaceFirst(tx *sql.Tx, base string, now int64) (*wire.SlotView, error) {
	cur, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND number=1`, base))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	drop := func() (*wire.SlotView, error) {
		if err := LogOffline(tx, base, 1, fromMS(now)); err != nil {
			return nil, err
		}
		_, err := tx.Exec(`DELETE FROM slots WHERE base=? AND number=1`, base)
		return nil, err
	}
	if ms(cur.LeaseExpiresAt) <= now {
		return drop()
	}
	n, err := freeNumber(tx, base, now, map[int]bool{1: true})
	if errors.Is(err, errSlotsFull) {
		return drop() // nowhere to go: the old holder is released
	}
	if err != nil {
		return nil, err
	}
	// Renumber in place so every column (presence included) moves along.
	if _, err = tx.Exec(`UPDATE slots SET number=? WHERE base=? AND number=1`, n, base); err != nil {
		return nil, err
	}
	if err = movePresenceLog(tx, base, 1, n, fromMS(now)); err != nil {
		return nil, err
	}
	cur.Number, cur.Role = n, identity.SlotRole(base, n)
	return &cur, nil
}

// renewSlot extends (or, with release, ends) the lease of the identity's slot.
// An expired slot whose number nobody took is renewed as well. The answer
// carries the identity's current role, which differs from the caller's when a
// takeover moved it.
func (s *Service) renewSlot(ctx context.Context, base string, req wire.SlotRequest, release bool) (wire.SlotGrant, error) {
	now := s.now()
	until := now.Add(s.LeaseLength())
	if release {
		until = now
	}
	var g wire.SlotGrant
	var box, handed string
	var moved int
	var told []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		own, has, err := identityRow(tx, base, req)
		if err != nil {
			return err
		}
		if !has {
			return errNoSlot
		}
		if !release {
			if told, err = endLeases(tx, base, req, now); err != nil {
				return err
			}
		}
		set, args := `lease_until=?`, []any{ms(until)}
		if release {
			// An intended stop: no offline notice (see stall.go).
			set += `,offline_noticed_at=?,subagents=0`
			args = append(args, ms(now))
		}
		if !release {
			set += `,session_id=?,label=?,parent=?`
			args = append(args, req.SessionID, req.Label, req.Parent)
		}
		if _, err = tx.Exec(`UPDATE slots SET `+set+` WHERE base=? AND number=?`, append(args, base, own.Number)...); err != nil {
			return err
		}
		if release {
			if err = LogOffline(tx, base, own.Number, now); err != nil {
				return err
			}
		}
		if !release {
			if err = applyPresence(tx, base, own.Number, req, now, false); err != nil {
				return err
			}
			if box, g.HeldDelivered, err = mail.DeliverHeld(tx, base, own.Number, req.Label, now); err != nil {
				return err
			}
			if handed, moved, err = mail.DeliverHandoff(tx, base, own.Number, req.Machine, req.Directory, req.Label, now); err != nil {
				return err
			}
		}
		g.Reclaimed = true
		return s.fillGrant(tx, &g, base, own.Number)
	})
	s.wakeHeld(err, box, g.HeldDelivered)
	s.wakeHeld(err, handed, moved)
	s.wakeTold(err, told)
	return g, err
}

func (s *Service) fillGrant(tx *sql.Tx, g *wire.SlotGrant, base string, number int) error {
	v, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND number=?`, base, number))
	if err != nil {
		return err
	}
	g.SlotView = v
	g.LeaseSeconds = int(s.LeaseLength() / time.Second)
	return tx.QueryRow(`SELECT COUNT(*) FROM slots WHERE base=? AND number<>? AND lease_until>?`, base, number, ms(s.now())).Scan(&g.OtherActive)
}

func slotRequestValid(q wire.SlotRequest) bool {
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
