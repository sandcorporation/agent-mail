package hosting

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

const maxBody = 64 << 10

// AdminTerminalsAPI serves /api/admin/terminals (rest "") and
// /api/admin/terminals/{id} (rest = id). The server has already identified
// the admin and applied the write guard. It reports false for a path it does
// not serve (the per-session actions of later tickets).
func (s *Service) AdminTerminalsAPI(w http.ResponseWriter, r *http.Request, rest string) bool {
	if strings.Contains(rest, "/") {
		return false
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			out, err := s.List()
			if err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return true
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			c, ok := decode[wire.HostedSessionCreate](w, r)
			if !ok {
				return true
			}
			h, err := s.Create(c)
			writeResult(w, http.StatusCreated, h, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	if !ValidID(rest) {
		http.Error(w, "no such hosted session", http.StatusNotFound)
		return true
	}
	switch r.Method {
	case http.MethodPut:
		c, ok := decode[wire.HostedSessionChange](w, r)
		if !ok {
			return true
		}
		h, err := s.Update(rest, c)
		writeResult(w, http.StatusOK, h, err)
	case http.MethodDelete:
		h, err := s.Delete(rest)
		writeResult(w, http.StatusOK, h, err)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	return true
}

// AdminNodesAPI serves GET /api/admin/nodes (rest "") and DELETE
// /api/admin/nodes/{name} (rest = name): revoke, which also closes the node's
// socket. The server has already identified the admin and applied the write
// guard. It reports false for a path it does not serve.
func (h *Hub) AdminNodesAPI(w http.ResponseWriter, r *http.Request, rest string) bool {
	if strings.Contains(rest, "/") {
		return false
	}
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		out, err := h.svc.Nodes()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return true
		}
		writeJSON(w, http.StatusOK, out)
		return true
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	n, err := h.Revoke(rest)
	switch {
	case errors.Is(err, ErrNoSuchNode):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, "update failed", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, n)
	}
	return true
}

// joinAPI serves POST /api/node/join: an invite code for the node's name and
// token. The code is the credential; no bearer token is asked for.
func (s *Service) joinAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var j wire.NodeJoin
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&j); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := s.JoinNode(j.Code)
	switch {
	case errors.Is(err, ErrInviteExpired), errors.Is(err, ErrInviteInvalid):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, ErrNodeExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, "join failed", http.StatusInternalServerError)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}
}

// decode reads a JSON body of type T; unknown fields are refused so a
// misspelt field is not silently dropped.
func decode[T wire.HostedSessionCreate | wire.HostedSessionChange](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return v, false
	}
	return v, true
}

func writeResult(w http.ResponseWriter, ok int, h wire.HostedSession, err error) {
	var invalid InvalidError
	switch {
	case err == nil:
		writeJSON(w, ok, h)
	case errors.As(err, &invalid):
		http.Error(w, invalid.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrDuplicate):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, "update failed", http.StatusInternalServerError)
	}
}

func writeJSON[T wire.HostedSession | []wire.HostedSession | wire.NodeView | []wire.NodeView | wire.NodeJoined](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
