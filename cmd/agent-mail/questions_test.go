package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func adminRequest(s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Header.Set("X-Agent-Mail-Admin", "proxy-secret")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

var dashboardHeaders = map[string]string{"Content-Type": "application/json", "X-Agent-Mail-Dashboard": "1", "Sec-Fetch-Site": "same-origin"}

func adminWrite(s *Server, method, path, body string) *httptest.ResponseRecorder {
	return adminRequest(s, method, path, body, dashboardHeaders)
}

func putSettings(t *testing.T, s *Server, target, body string) questionSettings {
	t.Helper()
	w := adminWrite(s, "PUT", "/api/admin/question-settings/"+target, body)
	if w.Code != 200 {
		t.Fatalf("PUT %s=%d %s", target, w.Code, w.Body)
	}
	var v questionSettings
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQuestionSettingsMigrationCreatesDefaultIdempotently(t *testing.T) {
	s := testServer(t)
	s.adminProxyToken = "proxy-secret"
	putSettings(t, s, "default", `{"can_ask":true,"watchers":["a"],"discord":true}`)
	if err := initSchema(s.db); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM question_settings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	v, err := s.resolveQuestionSettings("anyone")
	if err != nil || v.Target != "default" || !v.CanAsk || !v.Discord || len(v.Watchers) != 1 {
		t.Fatalf("migration overwrote default: %+v %v", v, err)
	}
}

func TestQuestionSettingsDefaultRowIsFalse(t *testing.T) {
	s := testServer(t)
	v, err := s.resolveQuestionSettings("svc/coordinator-2")
	if err != nil || v.Target != "default" || v.CanAsk || v.Discord || v.Watchers == nil || len(v.Watchers) != 0 || v.UpdatedAt == "" {
		t.Fatalf("default=%+v %v", v, err)
	}
}

func TestQuestionSettingsPrecedence(t *testing.T) {
	s := testServer(t)
	s.adminProxyToken = "proxy-secret"
	putSettings(t, s, "svc/*", `{"can_ask":true}`)
	putSettings(t, s, "svc/team/*", `{"discord":true}`)
	putSettings(t, s, "svc/team/lead", `{"watchers":["boss"]}`)
	cases := map[string]string{
		"svc/coordinator":     "svc/*",
		"svc/team/worker":     "svc/team/*",
		"svc/team/worker-3":   "svc/team/*",
		"svc/team/lead":       "svc/team/lead",
		"svc/team/lead-2":     "svc/team/lead",
		"other/svc/team/lead": "default",
		"svc":                 "default",
		"svcx/a":              "default",
	}
	for role, want := range cases {
		v, err := s.resolveQuestionSettings(role)
		if err != nil || v.Target != want {
			t.Errorf("%s matched %q (%v), want %q", role, v.Target, err, want)
		}
	}
	v, _ := s.resolveQuestionSettings("svc/team/lead-2")
	if v.CanAsk || v.Discord || len(v.Watchers) != 1 || v.Watchers[0] != "boss" {
		t.Fatalf("exact entry values=%+v", v)
	}
}

func TestQuestionSettingsAdminAPI(t *testing.T) {
	s := testServer(t)
	s.adminProxyToken = "proxy-secret"
	v := putSettings(t, s, "svc/coordinator", `{"can_ask":true,"watchers":["team/boss","team/boss"],"discord":false}`)
	if v.Target != "svc/coordinator" || !v.CanAsk || len(v.Watchers) != 1 || v.UpdatedAt == "" {
		t.Fatalf("put response=%+v", v)
	}
	putSettings(t, s, "svc/*", `{"can_ask":false,"watchers":[],"discord":true}`)
	w := adminRequest(s, "GET", "/api/admin/question-settings", "", nil)
	var list []questionSettings
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 3 || list[0].Target != "default" {
		t.Fatalf("list=%d %s", w.Code, w.Body)
	}

	tooMany := make([]string, 21)
	for i := range tooMany {
		tooMany[i] = "r" + strings.Repeat("x", i+1)
	}
	many, _ := json.Marshal(map[string]any{"watchers": tooMany})
	bad := []struct{ target, body string }{
		{"svc/*/x", `{}`},
		{"*", `{}`},
		{"bad%20name", `{}`},
		{"svc/x", `{"watchers":["bad name"]}`},
		{"svc/x", string(many)},
		{"svc/x", `{"can_ask":"yes"}`},
		{"svc/x", `{"unknown":1}`},
		{"svc/x", `not json`},
	}
	for _, c := range bad {
		if w := adminWrite(s, "PUT", "/api/admin/question-settings/"+c.target, c.body); w.Code != 400 {
			t.Errorf("PUT %s %s=%d", c.target, c.body, w.Code)
		}
	}

	if w := adminWrite(s, "DELETE", "/api/admin/question-settings/default", ""); w.Code != 400 {
		t.Fatalf("delete default=%d", w.Code)
	}
	if w := adminWrite(s, "DELETE", "/api/admin/question-settings/svc/*", ""); w.Code != 200 {
		t.Fatalf("delete prefix=%d %s", w.Code, w.Body)
	}
	if w := adminWrite(s, "DELETE", "/api/admin/question-settings/svc/*", ""); w.Code != 404 {
		t.Fatalf("delete missing=%d", w.Code)
	}
	if v, _ := s.resolveQuestionSettings("svc/worker"); v.Target != "default" {
		t.Fatalf("after delete matched %q", v.Target)
	}
}

func TestAdminWriteGuard(t *testing.T) {
	s := testServer(t)
	s.adminProxyToken = "proxy-secret"
	path := "/api/admin/question-settings/svc/x"
	body := `{"can_ask":true}`
	without := func(drop string, override map[string]string) map[string]string {
		h := map[string]string{}
		for k, v := range dashboardHeaders {
			if k != drop {
				h[k] = v
			}
		}
		for k, v := range override {
			h[k] = v
		}
		return h
	}
	forbidden := map[string]map[string]string{
		"no dashboard header": without("X-Agent-Mail-Dashboard", nil),
		"no content type":     without("Content-Type", nil),
		"wrong content type":  without("", map[string]string{"Content-Type": "text/plain"}),
		"cross-site":          without("", map[string]string{"Sec-Fetch-Site": "cross-site"}),
		"same-site":           without("", map[string]string{"Sec-Fetch-Site": "same-site"}),
	}
	for name, h := range forbidden {
		for _, method := range []string{"PUT", "POST", "DELETE"} {
			if w := adminRequest(s, method, path, body, h); w.Code != 403 {
				t.Errorf("%s %s=%d", name, method, w.Code)
			}
		}
	}
	if w := adminRequest(s, "PUT", path, body, without("Sec-Fetch-Site", nil)); w.Code != 200 {
		t.Fatalf("no Sec-Fetch-Site=%d %s", w.Code, w.Body)
	}
	if w := adminRequest(s, "PUT", path, body, without("", map[string]string{"Content-Type": "application/json; charset=utf-8"})); w.Code != 200 {
		t.Fatalf("json with charset=%d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("PUT", path, bytes.NewBufferString(body))
	for k, v := range dashboardHeaders {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 401 {
		t.Fatalf("guarded write without proxy secret=%d", w.Code)
	}
	// GET needs no dashboard headers.
	if w := adminRequest(s, "GET", "/api/admin/roles", "", nil); w.Code != 200 {
		t.Fatalf("GET roles=%d", w.Code)
	}
	if w := adminRequest(s, "GET", "/api/admin/question-settings", "", nil); w.Code != 200 {
		t.Fatalf("GET settings=%d", w.Code)
	}
	if w := adminWrite(s, "POST", "/api/admin/roles", `{}`); w.Code != 405 {
		t.Fatalf("guarded write to read-only route=%d", w.Code)
	}
}
