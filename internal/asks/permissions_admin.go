package asks

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// AdminPermissionsAPI serves the dashboard's permission endpoints behind the
// admin auth (writes also behind the server's adminWriteAllowed, checked by
// adminAPI):
//
//	GET  /api/admin/permissions[?status=pending|closed|allowed|denied|expired][&limit=N]
//	GET  /api/admin/permissions/{id}
//	POST /api/admin/permissions/{id}/answer  {decision, note}
//
// The list is newest first (default 100, at most 500); closed is every
// request that is no longer pending. An answer goes through AnswerPermission
// as PermissionByDashboard: no boss check, the same row, mail and JSON as
// POST /api/permissions/{id}/answer.
func (s *Service) AdminPermissionsAPI(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.adminListPermissions(w, r)
	case len(parts) == 1 && r.Method == http.MethodGet:
		v, err := s.permissionByID(parts[0])
		var pe permissionErr
		switch {
		case errors.As(err, &pe):
			http.Error(w, pe.msg, pe.code)
		case err != nil:
			http.Error(w, "could not load request", 500)
		default:
			writeJSON(w, adminPermission(v))
		}
	case len(parts) == 2 && parts[0] != "" && parts[1] == "answer" && r.Method == http.MethodPost:
		s.answerPermissionHTTP(w, r, parts[0], PermissionByDashboard, nil)
	case rest == "" || len(parts) <= 2:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// adminPermission is the dashboard view: every stored field, and the
// dashboard may answer any pending request.
func adminPermission(v wire.PermissionView) wire.AdminPermission {
	v.CanAnswer = v.Status == "pending"
	return wire.AdminPermission{PermissionView: v, RequesterBaseOut: v.RequesterBase, ParentBaseOut: v.ParentBase, DeclaredParentOut: v.DeclaredParent}
}

func (s *Service) adminListPermissions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, "limit must be 1-500", 400)
			return
		}
		limit = n
	}
	now := ms(s.now())
	where, args := "", []any{}
	switch q.Get("status") {
	case "":
	case "pending":
		where, args = ` WHERE decision='' AND expires_at>?`, []any{now}
	case "closed":
		where, args = ` WHERE decision<>'' OR expires_at<=?`, []any{now}
	case "allowed":
		where = ` WHERE decision='allow'`
	case "denied":
		where = ` WHERE decision='deny'`
	case "expired":
		where, args = ` WHERE decision='' AND expires_at<=?`, []any{now}
	default:
		http.Error(w, "status must be pending, closed, allowed, denied or expired", 400)
		return
	}
	rows, err := s.db.Query(`SELECT `+permissionCols()+` FROM permissions`+where+` ORDER BY created_at DESC, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		http.Error(w, "could not list requests", 500)
		return
	}
	defer rows.Close()
	out := wire.AdminPermissionList{Permissions: []wire.AdminPermission{}}
	for rows.Next() {
		v, err := scanPermission(rows, s.now())
		if err != nil {
			http.Error(w, "could not list requests", 500)
			return
		}
		out.Permissions = append(out.Permissions, adminPermission(v))
	}
	if rows.Err() != nil {
		http.Error(w, "could not list requests", 500)
		return
	}
	writeJSON(w, out)
}
