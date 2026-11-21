package mail

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
)

// revokedListMax is how many mails a revoke notice lists before "… and N more".
const revokedListMax = 50

type revokedMail struct {
	id                         int64
	sender, recipient, subject string
	created                    string
}

// NoticeRevoked tells every sender with pending mail to role (or one of its
// <role>-N mailboxes) that the mail will not be read: one system mail per
// sender, idem revoked:<role>:<stamp>:<sender>. Mail from the system and
// from role itself is left out. The mail is kept as it is. It returns the
// mailboxes it wrote to.
func NoticeRevoked(tx *sql.Tx, role, stamp string, now time.Time) ([]string, error) {
	rows, err := tx.Query(`SELECT m.id,m.sender,m.recipient,m.subject,m.created_at FROM messages m `+StatusJoin+
		` WHERE (m.recipient=? OR substr(m.recipient,1,?)=?) AND m.sender NOT IN (?,?) AND `+StatusExpr+`='pending' ORDER BY m.id`,
		role, len(role)+1, role+"-", identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return nil, err
	}
	var all []revokedMail
	for rows.Next() {
		var m revokedMail
		if err = rows.Scan(&m.id, &m.sender, &m.recipient, &m.subject, &m.created); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// BaseOf queries the database: only after the rows are closed.
	bySender := map[string][]revokedMail{}
	var senders []string
	for _, m := range all {
		if identity.BaseOf(tx, m.recipient) != role || identity.BaseOf(tx, m.sender) == role {
			continue
		}
		if bySender[m.sender] == nil {
			senders = append(senders, m.sender)
		}
		bySender[m.sender] = append(bySender[m.sender], m)
	}
	at := stamp
	if t, e := time.Parse(time.RFC3339Nano, stamp); e == nil {
		at = t.UTC().Format(time.RFC3339)
	}
	var boxes []string
	for _, sender := range senders {
		stored, err := InsertSystemMailOnce(tx, sender, "recipient revoked: "+role, revokedBody(role, at, bySender[sender]), "", "normal", "revoked:"+role+":"+stamp+":"+sender, now)
		if err != nil {
			return nil, err
		}
		if stored {
			boxes = append(boxes, sender)
		}
	}
	return boxes, nil
}

func revokedBody(role, at string, list []revokedMail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Role %s was revoked at %s. %d pending message(s) you sent to it will not be read:\n", role, at, len(list))
	for i, m := range list {
		if i == revokedListMax {
			fmt.Fprintf(&b, "… and %d more\n", len(list)-i)
			break
		}
		created := m.created
		if t, e := time.Parse(time.RFC3339Nano, m.created); e == nil {
			created = t.UTC().Format(time.RFC3339)
		}
		b.WriteString("#" + strconv.FormatInt(m.id, 10) + " " + m.subject + " (" + m.recipient + ", sent " + created + ")\n")
	}
	b.WriteString("The messages are kept. Restoring the role makes them readable again.")
	return b.String()
}
