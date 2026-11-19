package hosting

import (
	"log"
	"regexp"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Slot tie-in (#189).
//
// A hosted session's identity is its slot: the switchboard plugin inside it
// registers (base, machine, directory, session id) exactly as a session
// launched by hand does, and gets its old number back because machine and
// directory stay the same across relaunches. The hub never registers or
// changes slots; it reads them through SlotOf to show the role with its
// number and presence, and to follow the conversation: when the plugin
// reports a conversation other than the record's (a /clear inside the
// terminal), the record takes it, so the next relaunch resumes it.

// Slot is what SlotOf reports of a slot.
type Slot struct {
	Role      string // the numbered role, e.g. team/worker-1
	Directory string // as registered, possibly "<dir> #<8 hex>"
	SessionID string // the conversation the plugin last reported
	// LeaseUntil is when the lease ends; Seen is the last register or
	// heartbeat. Live is LeaseUntil in the future.
	LeaseUntil time.Time
	Seen       time.Time
	Live       bool
}

// SlotOf finds the slot of base held at machine in directory, the plugin's
// session-marked form "<directory> #<8 hex>" included; ok is false when
// there is none. The server builds it on presence.
type SlotOf func(base, machine, directory string) (s Slot, ok bool, err error)

// SetSlotOf gives the hub its slot reader. Without one the list carries no
// slot and no conversation is followed.
func (h *Hub) SetSlotOf(fn SlotOf) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slotOf = fn
}

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidSessionID reports whether sid is an agent conversation id (a UUID).
func ValidSessionID(sid string) bool { return sessionIDPattern.MatchString(sid) }

// followCandidate is a record whose conversation may be followed: launched,
// running on an attached node, with the host time its current run was
// first reported.
type followCandidate struct {
	rec wire.HostedSession
	run int64
}

// follow adopts the conversations running sessions moved to inside their
// terminals. A record follows its slot when the slot is leased, was heard
// from at or after the host first saw the current run (so a slot the
// previous process left behind never counts), and reports a valid
// conversation id other than the record's. The record keeps launched, so
// its node keeps the process and the next relaunch resumes the new id. It
// reports whether a record changed.
func (h *Hub) follow(recs []wire.HostedSession) bool {
	h.mu.Lock()
	slotOf := h.slotOf
	var cands []followCandidate
	for _, r := range recs {
		rt, reported := h.runtime[r.ID]
		_, attached := h.conns[r.Node]
		run, seen := h.runSeen[r.ID]
		if r.Launched && reported && attached && seen && rt.State == stateRunning {
			cands = append(cands, followCandidate{rec: r, run: run})
		}
	}
	h.mu.Unlock()
	if slotOf == nil || len(cands) == 0 {
		return false
	}
	machines, err := h.svc.nodeMachines()
	if err != nil {
		log.Printf("hosting: follow: %v", err)
		return false
	}
	changed := false
	for _, c := range cands {
		r, machine := c.rec, machines[c.rec.Node]
		if machine == "" {
			continue
		}
		sl, ok, err := slotOf(r.Base, machine, r.Directory)
		if err != nil {
			log.Printf("hosting: follow %s: %v", r.ID, err)
			continue
		}
		if !ok || !sl.Live || sl.SessionID == r.AgentSessionID || !ValidSessionID(sl.SessionID) || sl.Seen.UnixMilli() < c.run {
			continue
		}
		adopted, err := h.svc.adoptConversation(r.ID, r.AgentSessionID, sl.SessionID)
		if err != nil {
			log.Printf("hosting: follow %s: %v", r.ID, err)
			continue
		}
		changed = changed || adopted
	}
	return changed
}

// withSlots fills each record's Slot and Presence (Hub.List).
func (h *Hub) withSlots(recs []wire.HostedSession) {
	h.mu.Lock()
	slotOf := h.slotOf
	h.mu.Unlock()
	if slotOf == nil || len(recs) == 0 {
		return
	}
	machines, err := h.svc.nodeMachines()
	if err != nil {
		log.Printf("hosting: slots: %v", err)
		return
	}
	for i, r := range recs {
		alive := r.Runtime.State == stateRunning
		recs[i].Presence = wire.PresenceOffline
		if alive {
			recs[i].Presence = wire.PresenceProcessAlive
		}
		machine := machines[r.Node]
		if machine == "" {
			continue
		}
		sl, ok, err := slotOf(r.Base, machine, r.Directory)
		if err != nil {
			log.Printf("hosting: slot of %s: %v", r.ID, err)
			continue
		}
		if !ok {
			continue
		}
		recs[i].Slot = &wire.HostedSlot{Role: sl.Role, Directory: sl.Directory, SessionID: sl.SessionID, LeaseUntil: sl.LeaseUntil.UnixMilli(), Online: sl.Live}
		if sl.Live {
			recs[i].Presence = wire.PresenceOnline
		}
	}
}

// stateRunning is the node's observed state of a live process.
const stateRunning = "running"

// nodeMachines is each known node's machine name (from its hello), by
// node name.
func (s *Service) nodeMachines() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name,machine FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, machine string
		if err := rows.Scan(&name, &machine); err != nil {
			return nil, err
		}
		out[name] = machine
	}
	return out, rows.Err()
}

// adoptConversation moves a launched record from conversation old to sid,
// keeping it launched; it does nothing (false) when the record no longer
// holds old or is not launched.
func (s *Service) adoptConversation(id, old, sid string) (bool, error) {
	res, err := s.db.Exec(`UPDATE hosted_sessions SET agent_session_id=?,updated_at=? WHERE id=? AND agent_session_id=? AND launched=1`,
		sid, s.clock().UnixMilli(), id, old)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
