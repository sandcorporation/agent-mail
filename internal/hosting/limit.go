package hosting

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Session cap and subscription limit (#190).

// Limit sources.
const (
	LimitOperator = "operator"
	LimitScreen   = "screen"
)

// LimitClearedKind is the live change the host publishes once each time the
// subscription limit clears; #174 resumes queued work items on it.
const LimitClearedKind = "limit-cleared"

// CapacityError refuses a record that would run beyond its node's cap.
type CapacityError struct {
	Node string
	Max  int
}

func (e CapacityError) Error() string {
	return fmt.Sprintf("node %s already runs its cap of %d sessions (%s%s); stop one first",
		e.Node, e.Max, config.EnvPrefix, config.HostingMaxName)
}

func checkClass(v string) error {
	switch v {
	case wire.ClassLead, wire.ClassCoordinator, wire.ClassExecute, wire.ClassReview:
		return nil
	}
	return invalidf("class: want lead, coordinator, execute or review, not %q", v)
}

// AcceptsWork is the rule for wire.HostedSession.AcceptingWork: while the
// subscription limit holds, only lead and coordinator sessions take new work
// items.
func AcceptsWork(class string, limited bool) bool {
	return !limited || class == wire.ClassLead || class == wire.ClassCoordinator
}

func (s *Service) acceptingWork(class string) bool {
	l, err := s.Limit()
	return err != nil || AcceptsWork(class, l.Limited)
}

// NodeMax is the cap node reported in its hello, or
// config.DefaultHostingMax before it did.
func (s *Service) NodeMax(node string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT max_sessions FROM nodes WHERE name=?`, node).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if n <= 0 {
		n = config.DefaultHostingMax
	}
	return n, nil
}

// checkCap refuses one more record set to run on node (other than except)
// when as many as its cap already are. Records set to run count whatever
// their observed state, so a record never waits behind the cap because of
// an add or a start.
func (s *Service) checkCap(node, except string) error {
	max, err := s.NodeMax(node)
	if err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hosted_sessions WHERE node=? AND desired=? AND id<>?`, node, DesiredRunning, except).Scan(&n); err != nil {
		return err
	}
	if n >= max {
		return CapacityError{Node: node, Max: max}
	}
	return nil
}

// Limit is the subscription limit now: the operator's switch, else the
// newest limit a node's screens show that began after the last clear and
// whose reset time has not passed.
func (s *Service) Limit() (wire.HostingLimit, error) {
	var out wire.HostingLimit
	var op, setAt, cleared int64
	err := s.db.QueryRow(`SELECT operator,set_at,cleared_at FROM hosting_limit WHERE id=1`).Scan(&op, &setAt, &cleared)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if cleared > 0 {
		out.ClearedAt = &cleared
	}
	if op != 0 {
		out.Limited, out.Source, out.Since = true, LimitOperator, &setAt
		return out, nil
	}
	now := s.clock().UnixMilli()
	rows, err := s.db.Query(`SELECT limit_since,limit_reset FROM nodes WHERE limit_since IS NOT NULL AND limit_since>? AND revoked_at IS NULL AND (limit_reset IS NULL OR limit_reset>?)`, cleared, now)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var since, reset int64
	for rows.Next() {
		var sn int64
		var rs sql.NullInt64
		if err := rows.Scan(&sn, &rs); err != nil {
			return out, err
		}
		since = max(since, sn)
		if rs.Valid {
			reset = max(reset, rs.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if since > 0 {
		out.Limited, out.Source, out.Since = true, LimitScreen, &since
		if reset > 0 {
			out.ResetAt = &reset
		}
	}
	return out, nil
}

// SetLimit is the operator's switch. Off also clears a limit the screens
// show: a showing that began before now is ignored from then on.
func (s *Service) SetLimit(on bool) (wire.HostingLimit, error) {
	now := s.clock().UnixMilli()
	var err error
	if on {
		_, err = s.db.Exec(`INSERT INTO hosting_limit(id,operator,set_at) VALUES(1,1,?) ON CONFLICT(id) DO UPDATE SET operator=1,set_at=CASE WHEN operator=1 THEN set_at ELSE excluded.set_at END`, now)
	} else {
		_, err = s.db.Exec(`INSERT INTO hosting_limit(id,operator,cleared_at) VALUES(1,0,?) ON CONFLICT(id) DO UPDATE SET operator=0,cleared_at=excluded.cleared_at`, now)
	}
	if err != nil {
		return wire.HostingLimit{}, err
	}
	return s.Limit()
}

// markCleared records when the limit cleared (a reset time passed or the
// screens stopped showing it), for ClearedAt.
func (s *Service) markCleared(at int64) error {
	_, err := s.db.Exec(`INSERT INTO hosting_limit(id,cleared_at) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET cleared_at=max(cleared_at,excluded.cleared_at)`, at)
	return err
}

// setNodeLimit stores the limit node's screens show (#190).
func (s *Service) setNodeLimit(node string, l wire.NodeLimit) error {
	var since, reset any
	if l.Limited {
		since = l.Since
		if l.ResetAt != nil {
			reset = *l.ResetAt
		}
	}
	_, err := s.db.Exec(`UPDATE nodes SET limit_since=?,limit_reset=? WHERE name=?`, since, reset, node)
	return err
}

// WorkKey is what ResumeOrder needs of a work item: when it was queued
// because no session took it (zero: not queued) and when it arrived.
type WorkKey struct {
	QueuedAt  int64
	ArrivedAt int64
}

// ResumeOrder sorts work items for dispatch once the subscription limit
// clears (LimitClearedKind): items queued while it held come first, oldest
// queued first; new items follow in arrival order. The sort is stable. #174
// dispatches work items in this order.
func ResumeOrder[T any](items []T, key func(T) WorkKey) {
	slices.SortStableFunc(items, func(a, b T) int {
		ka, kb := key(a), key(b)
		qa, qb := ka.QueuedAt != 0, kb.QueuedAt != 0
		switch {
		case qa && !qb:
			return -1
		case qb && !qa:
			return 1
		case qa:
			if c := cmp.Compare(ka.QueuedAt, kb.QueuedAt); c != 0 {
				return c
			}
		}
		return cmp.Compare(ka.ArrivedAt, kb.ArrivedAt)
	})
}
