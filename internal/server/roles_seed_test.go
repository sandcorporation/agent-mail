package server

import (
	"database/sql"

	"github.com/sandcorporation/agent-mail/internal/store"
)

// seedRoles inserts role rows with plain tokens the way an old binary would,
// then hashes them as the startup migration does.
func seedRoles(db *sql.DB, query string, args ...any) (sql.Result, error) {
	res, err := db.Exec(query, args...)
	if err != nil {
		return res, err
	}
	return res, store.Init(db)
}
