package presence

import (
	"database/sql"
	"errors"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// AskingSlot is the live slot role names under base: role is base itself
// (slot 1) or <base>-N. ok is false when no live slot holds that number.
func AskingSlot(tx *sql.Tx, base, role string, now time.Time) (wire.SlotView, bool, error) {
	number := 1
	if role != base {
		b, n, ok := wire.SplitSlot(role)
		if !ok || b != base {
			return wire.SlotView{}, false, nil
		}
		number = n
	}
	v, err := scanSlot(tx.QueryRow(`SELECT `+slotCols()+` FROM slots WHERE base=? AND number=? AND lease_until>?`, base, number, ms(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return wire.SlotView{}, false, nil
	}
	return v, err == nil, err
}

// AnswerSlot is the role of the live slot of base that takes mail meant for
// the session asking was: that identity (machine and directory) under
// whatever number it holds now, else the most recently registered live slot
// with the same label (no label matches no label). ok is false when none is
// live: a session with another label works on something else.
func AnswerSlot(tx *sql.Tx, base string, asking wire.SlotView, now time.Time) (string, bool, error) {
	var n int
	err := tx.QueryRow(`SELECT number FROM slots WHERE base=? AND lease_until>? AND ((machine=? AND directory=?) OR label=?)
ORDER BY (machine=? AND directory=?) DESC, registered_at DESC, number DESC LIMIT 1`,
		base, ms(now), asking.Machine, asking.Directory, asking.Label, asking.Machine, asking.Directory).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return identity.SlotRole(base, n), true, nil
}
