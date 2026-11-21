package hosting

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// autosAPI serves /api/admin/terminals/{id}/autos… (rest after "autos",
// with its leading slash): the scheduled runs of one hosted session. The
// server has already identified the admin and applied the write guard.
//
//	GET    …/autos                list
//	POST   …/autos                add (wire.HostAutoCreate)
//	GET    …/autos/{aid}[?runs=N] the record and its newest N runs
//	DELETE …/autos/{aid}          remove
//	POST   …/autos/{aid}/run      run once now
//	POST   …/autos/{aid}/pause    stop firing
//	POST   …/autos/{aid}/resume   fire again
func (h *Hub) autosAPI(w http.ResponseWriter, r *http.Request, id, rest string) {
	if !ValidID(id) {
		http.Error(w, "no such hosted session", http.StatusNotFound)
		return
	}
	if _, err := h.svc.Get(id); err != nil {
		autoError(w, err)
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			out, err := h.svc.Autos(id)
			if err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
			}
			writeAuto(w, http.StatusOK, out)
		case http.MethodPost:
			var c wire.HostAutoCreate
			dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
				return
			}
			a, err := h.svc.CreateAuto(id, c)
			if err != nil {
				autoError(w, err)
				return
			}
			h.publish("terminals")
			writeAuto(w, http.StatusCreated, a)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	aidText, action, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	aid, err := strconv.ParseInt(aidText, 10, 64)
	if err != nil || aid <= 0 {
		http.Error(w, ErrNoSuchAuto.Error(), http.StatusNotFound)
		return
	}
	if a, err := h.svc.GetAuto(aid); err != nil || a.Session != id {
		autoError(w, ErrNoSuchAuto)
		return
	}
	if action == "" {
		switch r.Method {
		case http.MethodGet:
			n := 20
			if v := r.URL.Query().Get("runs"); v != "" {
				if n, err = strconv.Atoi(v); err != nil || n < 1 || n > AutoRunsKept {
					http.Error(w, "runs: want 1 to "+strconv.Itoa(AutoRunsKept), http.StatusBadRequest)
					return
				}
			}
			v, err := h.svc.AutoView(aid, n)
			if err != nil {
				autoError(w, err)
				return
			}
			writeAuto(w, http.StatusOK, v)
		case http.MethodDelete:
			a, err := h.svc.DeleteAuto(aid)
			if err != nil {
				autoError(w, err)
				return
			}
			h.publish("terminals")
			writeAuto(w, http.StatusOK, a)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var a wire.HostAuto
	switch action {
	case "run":
		run, err := h.RunAuto(aid)
		if err != nil {
			autoError(w, err)
			return
		}
		writeAuto(w, http.StatusOK, run)
		return
	case "pause":
		a, err = h.svc.PauseAuto(aid)
	case "resume":
		a, err = h.svc.ResumeAuto(aid)
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		autoError(w, err)
		return
	}
	h.publish("terminals")
	writeAuto(w, http.StatusOK, a)
}

func autoError(w http.ResponseWriter, err error) {
	var invalid InvalidError
	switch {
	case errors.As(err, &invalid):
		http.Error(w, invalid.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoSuchAuto):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, "update failed", http.StatusInternalServerError)
	}
}

func writeAuto[T wire.HostAuto | []wire.HostAuto | wire.HostAutoView | wire.HostAutoRun](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
