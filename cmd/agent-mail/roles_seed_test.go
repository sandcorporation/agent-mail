package main

import "database/sql"

// seedRoles inserts role rows with plain tokens the way an old binary would,
// then hashes them as the startup migration does.
func seedRoles(db *sql.DB, query string, args ...any) (sql.Result, error) {
	res, err := db.Exec(query, args...)
	if err != nil {
		return res, err
	}
	return res, migrateRoleTokens(db)
}
