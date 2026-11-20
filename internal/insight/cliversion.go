package insight

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Old CLI mail (#308).
//
// Once per UTC day and machine, a machine whose online sessions run a CLI
// below the server minimum, or one that sends no version, is mailed to every
// boss role from the system sender: "[옛 판] <machine> switchboard vX (서버 vY)",
// listing the machine's roles and their versions. The alerts row (kind "cli",
// key the machine) and the mail are written in one transaction, so repeated
// sweeps send it once. Nothing is sent when every machine is current.

const cliAlertKind = "cli"

// SweepCLIBehind mails the machines list reports, at most once a day each,
// and returns the number of machines mailed. serverVersion goes in the text.
func (s *Service) SweepCLIBehind(ctx context.Context, list func() ([]wire.CLIBehind, error), serverVersion string) (int, error) {
	behind, err := list()
	if err != nil || len(behind) == 0 {
		return 0, err
	}
	now := s.now()
	day := alertDay(now)
	sent := 0
	for _, b := range behind {
		subject := fmt.Sprintf("[옛 판] %s switchboard %s (서버 %s)", b.Machine, b.Version, serverVersion)
		var lines []string
		for i, role := range b.Roles {
			lines = append(lines, "- "+role+" "+b.RoleVersions[i])
		}
		body := b.Machine + "\n" + strings.Join(lines, "\n")
		var told []string
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			boxes, _ := s.bossMailboxes(tx)
			if len(boxes) == 0 {
				return nil
			}
			res, err := tx.Exec(`INSERT OR IGNORE INTO alerts(kind,key,day,sent_at) VALUES(?,?,?,?)`, cliAlertKind, b.Machine, day, ms(now))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			task := "alert:" + cliAlertKind + ":" + b.Machine + ":" + day
			for _, box := range boxes {
				if err = mail.InsertSystemMail(tx, box, subject, body, task, "normal", task+":"+box, now); err != nil {
					return err
				}
			}
			told = boxes
			return nil
		})
		if err != nil {
			return sent, err
		}
		if len(told) > 0 {
			sent++
			for _, box := range told {
				s.wake(box)
			}
		}
	}
	return sent, nil
}
