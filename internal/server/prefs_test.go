package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/session"
)

// Preferences for new sessions are per user: changed and seen only by them.
func TestSessionPrefsArePerUser(t *testing.T) {
	s := newTestServer(t, Options{})
	bob, err := s.users.CreateUser("bob", "Bob", false)
	if err != nil {
		t.Fatal(err)
	}
	signIn := func(uid string) reqOpt {
		secret, err := s.users.NewSession(uid, "test", "")
		if err != nil {
			t.Fatal(err)
		}
		return cookie(sessionCookie, secret)
	}
	admin, member := signIn(s.owner), signIn(bob.ID)
	prefs := func(as reqOpt) session.SessionPrefs {
		t.Helper()
		w := s.do("GET", "/api/state", "", as)
		var st struct {
			Prefs   session.SessionPrefs `json:"prefs"`
			Efforts map[string][]string  `json:"efforts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != http.StatusOK {
			t.Fatalf("state: %d %s", w.Code, w.Body.String())
		}
		if len(st.Efforts["claude"]) == 0 {
			t.Errorf("state lists no efforts: %v", st.Efforts)
		}
		return st.Prefs
	}

	w := s.do("PATCH", "/api/me/session-prefs", `{"agents":{"claude":{"model":"opus","effort":"max"}},"instructions":"Admin's secret rules"}`, admin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"instructions":"Admin's secret rules"`) {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if p := prefs(admin); p.Instructions != "Admin's secret rules" || p.Agents["claude"].Effort != "max" {
		t.Errorf("admin's state: %+v", p)
	}
	if p := prefs(member); p.Instructions != "" || len(p.Agents) != 0 {
		t.Errorf("bob sees the admin's preferences: %+v", p)
	}
	if w := s.do("PATCH", "/api/me/session-prefs", `{"instructions":"Bob's"}`, member); w.Code != http.StatusOK {
		t.Fatalf("bob's patch: %d %s", w.Code, w.Body.String())
	}
	if p := prefs(admin); p.Instructions != "Admin's secret rules" {
		t.Errorf("bob's patch changed the admin's: %+v", p)
	}
	if w := s.do("PATCH", "/api/me/session-prefs", `{"agents":{"codex":{"effort":"max"}}}`, member); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `unknown effort \"max\" for codex`) {
		t.Errorf("codex effort max: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("PATCH", "/api/me/session-prefs", `{"instructions":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", w.Code)
	}
}
