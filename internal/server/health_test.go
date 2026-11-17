package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHealthIncludesVersion(t *testing.T) {
	old := version
	version = "abc1234"
	defer func() { version = old }()
	w := httptest.NewRecorder()
	health(w, httptest.NewRequest("GET", "/health", nil))
	var got struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if !got.OK || got.Version != "abc1234" {
		t.Fatalf("got %+v", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
}
