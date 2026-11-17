package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Per-role question settings. A target is "default", an exact role name
// (which also covers its numbered slots <role>-N) or a role prefix "<p>/*".

const (
	defaultQuestionTarget = "default"
	maxQuestionWatchers   = 20
)

type questionSettings struct {
	Target    string   `json:"target"`
	CanAsk    bool     `json:"can_ask"`
	Watchers  []string `json:"watchers"`
	Discord   bool     `json:"discord"`
	UpdatedAt string   `json:"updated_at"`
}

func migrateQuestionSettings(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS question_settings(target TEXT PRIMARY KEY,can_ask INTEGER NOT NULL DEFAULT 0,watchers TEXT NOT NULL DEFAULT '[]',discord INTEGER NOT NULL DEFAULT 0,updated_at TEXT NOT NULL)`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO question_settings(target,can_ask,watchers,discord,updated_at) VALUES(?,0,'[]',0,?)`, defaultQuestionTarget, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// validQuestionTarget accepts "default", a role name, or "<role prefix>/*".
func validQuestionTarget(t string) bool {
	if t == defaultQuestionTarget {
		return true
	}
	if p, ok := strings.CutSuffix(t, "/*"); ok {
		return validRole(p)
	}
	return validRole(t)
}

const questionCols = `target,can_ask,watchers,discord,updated_at`

func scanQuestionSettings(sc interface{ Scan(...any) error }) (questionSettings, error) {
	var v questionSettings
	var watchers string
	if err := sc.Scan(&v.Target, &v.CanAsk, &watchers, &v.Discord, &v.UpdatedAt); err != nil {
		return v, err
	}
	if json.Unmarshal([]byte(watchers), &v.Watchers) != nil || v.Watchers == nil {
		v.Watchers = []string{}
	}
	return v, nil
}

func loadQuestionSettings(q queryer, target string) (questionSettings, error) {
	return scanQuestionSettings(q.QueryRow(`SELECT `+questionCols+` FROM question_settings WHERE target=?`, target))
}

// resolveQuestionSettings returns the settings that apply to role, with the
// matched target: an exact role entry (the role itself, then its base when
// role is <base>-N), else the longest matching "<prefix>/*", else "default".
func (s *Server) resolveQuestionSettings(role string) (questionSettings, error) {
	return resolveQuestionSettingsIn(s.db, role)
}

// resolveQuestionSettingsIn is resolveQuestionSettings read through q, so it
// also works inside a transaction.
func resolveQuestionSettingsIn(q queryer, role string) (questionSettings, error) {
	candidates := []string{role}
	base := role
	if b, _, ok := splitSlot(role); ok {
		base = b
		candidates = append(candidates, base)
	}
	for i := len(base) - 1; i > 0; i-- {
		if base[i] == '/' {
			candidates = append(candidates, base[:i]+"/*")
		}
	}
	candidates = append(candidates, defaultQuestionTarget)
	for _, t := range candidates {
		if t != defaultQuestionTarget && !validQuestionTarget(t) {
			continue
		}
		v, err := loadQuestionSettings(q, t)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return questionSettings{}, err
		}
	}
	return questionSettings{}, sql.ErrNoRows
}

// adminWriteAllowed is the CSRF guard for every non-GET admin request. The
// custom header forces a CORS preflight, which a cross-site page cannot pass.
func adminWriteAllowed(r *http.Request) bool {
	ct := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(ct, "application/json") || r.Header.Get("X-Agent-Mail-Dashboard") != "1" {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func (s *Server) questionSettingsAPI(w http.ResponseWriter, r *http.Request, target string, hasTarget bool) {
	switch {
	case !hasTarget && r.Method == http.MethodGet:
		s.listQuestionSettings(w)
	case hasTarget && r.Method == http.MethodPut:
		s.putQuestionSettings(w, r, target)
	case hasTarget && r.Method == http.MethodDelete:
		s.deleteQuestionSettings(w, target)
	case hasTarget && r.Method == http.MethodGet:
		http.NotFound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listQuestionSettings(w http.ResponseWriter) {
	rows, err := s.db.Query(`SELECT `+questionCols+` FROM question_settings ORDER BY target<>?, target`, defaultQuestionTarget)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	out := []questionSettings{}
	for rows.Next() {
		v, err := scanQuestionSettings(rows)
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		out = append(out, v)
	}
	if rows.Err() != nil {
		http.Error(w, "query failed", 500)
		return
	}
	writeJSON(w, out)
}

func (s *Server) putQuestionSettings(w http.ResponseWriter, r *http.Request, target string) {
	if !validQuestionTarget(target) {
		http.Error(w, "invalid target", 400)
		return
	}
	var req struct {
		CanAsk   bool     `json:"can_ask"`
		Watchers []string `json:"watchers"`
		Discord  bool     `json:"discord"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil {
		http.Error(w, "invalid JSON body", 400)
		return
	}
	if len(req.Watchers) > maxQuestionWatchers {
		http.Error(w, "at most 20 watchers", 400)
		return
	}
	watchers, seen := []string{}, map[string]bool{}
	for _, role := range req.Watchers {
		if !validRole(role) {
			http.Error(w, "invalid watcher role", 400)
			return
		}
		if !seen[role] {
			seen[role] = true
			watchers = append(watchers, role)
		}
	}
	encoded, _ := json.Marshal(watchers)
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO question_settings(target,can_ask,watchers,discord,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(target) DO UPDATE SET can_ask=excluded.can_ask,watchers=excluded.watchers,discord=excluded.discord,updated_at=excluded.updated_at`, target, req.CanAsk, string(encoded), req.Discord, now); err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	v, err := loadQuestionSettings(s.db, target)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	writeJSON(w, v)
}

func (s *Server) deleteQuestionSettings(w http.ResponseWriter, target string) {
	if target == defaultQuestionTarget || !validQuestionTarget(target) {
		http.Error(w, "cannot delete this target", 400)
		return
	}
	res, err := s.db.Exec(`DELETE FROM question_settings WHERE target=?`, target)
	if err != nil {
		http.Error(w, "delete failed", 500)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, map[string]string{"target": target, "status": "deleted"})
}
