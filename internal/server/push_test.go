package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/push"
	"github.com/tattsun/collie/internal/session"
)

type pushed struct {
	path    string
	urgency string
	topic   string
}

// fakePushService stands in for FCM & co.: every request to an allowed
// push service host lands here.
func fakePushService(t *testing.T, ps *push.Service) chan pushed {
	t.Helper()
	got := make(chan pushed, 16)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- pushed{r.URL.Path, r.Header.Get("Urgency"), r.Header.Get("Topic")}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	ps.SetTransport(&http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})
	return got
}

func browserKeys(t *testing.T) string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	enc := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf(`{"p256dh":%q,"auth":%q}`, enc(k.PublicKey().Bytes()), enc(auth))
}

func TestWebPush(t *testing.T) {
	ps, err := push.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := fakePushService(t, ps)
	s := newTestServer(t, Options{Push: ps})
	if err := s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, ""); err != nil {
		t.Fatal(err)
	}
	phone, _ := s.users.NewSession(s.owner, "iPhone", "")
	_, phoneSess, _ := s.users.SessionUser(phone, "")
	me := cookie(sessionCookie, phone)
	origin := header("Origin", "https://collie.example.com")

	if w := s.do("GET", "/api/push", "", me); w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("key: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("GET", "/api/push", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("key without signing in: %d", w.Code)
	}
	bad := `{"endpoint":"https://169.254.169.254/latest","keys":` + browserKeys(t) + `}`
	if w := s.do("POST", "/api/push/subscriptions", bad, me, origin); w.Code != http.StatusBadRequest {
		t.Fatalf("endpoint outside the push services: %d %s", w.Code, w.Body.String())
	}
	endpoint := "https://fcm.googleapis.com/fcm/send/phone"
	w := s.do("POST", "/api/push/subscriptions", `{"endpoint":"`+endpoint+`","keys":`+browserKeys(t)+`,"lang":"ja"}`, me, origin)
	if w.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	var reg struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	sub, ok := ps.Get(s.owner, endpoint)
	if !ok || sub.ID != reg.ID || sub.Session != phoneSess.Hash || sub.Lang != "ja" || sub.Contact != "https://collie.example.com" {
		t.Fatalf("registration %+v (id %q)", sub, reg.ID)
	}

	st := s.m.Store()
	st.PutProject(model.Project{ID: "p1", Name: "webapp", OwnerUser: s.owner})
	sess := model.Session{ID: "0123456789abcdef", ProjectID: "p1", Owner: s.owner, Title: "Fix the login bug", Status: model.StatusWaiting}
	st.PutSession(sess)
	expect := func(what string) pushed {
		t.Helper()
		select {
		case p := <-got:
			return p
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: nothing pushed", what)
		}
		return pushed{}
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case p := <-got:
			t.Fatalf("%s: pushed %+v", what, p)
		case <-time.After(150 * time.Millisecond):
		}
	}

	s.notifySession(sess, "approval")
	if p := expect("approval"); p.path != "/fcm/send/phone" || p.urgency != "high" || p.topic != sess.ID {
		t.Fatalf("pushed %+v", p)
	}
	s.notifySession(sess, "done")
	if p := expect("done"); p.urgency != "normal" {
		t.Fatalf("pushed %+v", p)
	}

	// Collie open in front of the user on the phone: the page notifies itself.
	c := session.NewClient(s.owner, phoneSess.Hash, func() {})
	s.m.Hub().Add(c)
	c.SetForeground(true, reg.ID)
	s.notifySession(sess, "done")
	quiet("foreground")
	c.SetForeground(false, reg.ID)
	s.notifySession(sess, "done")
	expect("background")
	s.m.Hub().Remove(c)

	// Someone else's session never reaches this phone.
	st.PutProject(model.Project{ID: "p2", Name: "theirs", OwnerUser: "someone-else"})
	s.notifySession(model.Session{ID: "fedcba9876543210", ProjectID: "p2", Owner: "someone-else"}, "done")
	quiet("other user's session")

	// The test button.
	if w := s.do("POST", "/api/push/test", `{"endpoint":"`+endpoint+`"}`, me); w.Code != http.StatusOK {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	if p := expect("test"); p.urgency != "high" || p.topic != "" {
		t.Fatalf("test pushed %+v", p)
	}
	if w := s.do("POST", "/api/push/test", `{"endpoint":"https://fcm.googleapis.com/fcm/send/other"}`, me); w.Code != http.StatusNotFound {
		t.Fatalf("test of an unregistered browser: %d", w.Code)
	}

	// Signing the phone out ends its notifications.
	s.users.DeleteSession(phoneSess.Hash)
	s.notifySession(sess, "error")
	quiet("signed out")
	if _, ok := ps.Get(s.owner, endpoint); ok {
		t.Fatal("the signed-out browser's registration was kept")
	}

	// Unsubscribing.
	laptop, _ := s.users.NewSession(s.owner, "Mac", "")
	lc := cookie(sessionCookie, laptop)
	ep2 := "https://updates.push.services.mozilla.com/wpush/v2/laptop"
	if w := s.do("POST", "/api/push/subscriptions", `{"endpoint":"`+ep2+`","keys":`+browserKeys(t)+`}`, lc, origin); w.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("POST", "/api/push/unsubscribe", `{"endpoint":"`+ep2+`"}`, lc); w.Code != http.StatusOK {
		t.Fatalf("unsubscribe: %d", w.Code)
	}
	if len(ps.ForUser(s.owner)) != 0 {
		t.Fatal("unsubscribe kept the registration")
	}
}

// The service worker re-registers a renewed subscription; it knows no
// language, so the old registration's is kept.
func TestWebPushRenewedSubscription(t *testing.T) {
	ps, _ := push.Open(t.TempDir())
	s := newTestServer(t, Options{Push: ps})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, "")
	secret, _ := s.users.NewSession(s.owner, "iPhone", "")
	me := cookie(sessionCookie, secret)
	old := "https://web.push.apple.com/old"
	if w := s.do("POST", "/api/push/subscriptions", `{"endpoint":"`+old+`","keys":`+browserKeys(t)+`,"lang":"ja"}`, me); w.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("POST", "/api/push/subscriptions", `{"endpoint":"https://web.push.apple.com/new","keys":`+browserKeys(t)+`,"replaces":"`+old+`"}`, me); w.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", w.Code, w.Body.String())
	}
	list := ps.ForUser(s.owner)
	if len(list) != 1 || list[0].Endpoint != "https://web.push.apple.com/new" || list[0].Lang != "ja" {
		t.Fatalf("after renewal: %+v", list)
	}
}

func TestPushPayloadCarriesNoContent(t *testing.T) {
	sess := model.Session{ID: "s1", Title: "Fix the login bug", Preview: "secret preview of the reply", Cwd: "/srv/x", Branch: "b"}
	var m map[string]any
	if err := json.Unmarshal(sessionPayload(sess, "webapp", "done", "en"), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"sid": "s1", "project": "webapp", "title": "Fix the login bug", "status": "done", "lang": "en"}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Fatalf("payload %v", m)
	}
}
