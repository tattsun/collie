package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example of RFC 8291 Appendix A, byte for byte.
func TestEncryptRFC8291Example(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptWith(
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		[]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"),
		as,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64.EncodeToString(got) != want {
		t.Fatalf("body\n got %s\nwant %s", b64.EncodeToString(got), want)
	}
}

// browser is the receiving end of a subscription.
type browser struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) browser {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return browser{k, a}
}

func (b browser) sub(user, endpoint string) Subscription {
	return Subscription{UserID: user, Endpoint: endpoint, P256dh: b64.EncodeToString(b.key.PublicKey().Bytes()), Auth: b64.EncodeToString(b.auth)}
}

// decrypt does what the browser does with a push body.
func (b browser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("short body")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	asPub, ct := body[21:21+idlen], body[21+idlen:]
	if rs != recordSize || len(ct) > int(rs) {
		t.Fatalf("record size %d, ciphertext %d", rs, len(ct))
	}
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := b.key.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, b.auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(b.key.PublicKey().Bytes())+string(asPub), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(pt) == 0 || pt[len(pt)-1] != 0x02 {
		t.Fatalf("missing last-record delimiter")
	}
	return pt[:len(pt)-1]
}

func TestOpenGeneratesAndKeepsKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k := mustB64(t, s.PublicKey()); len(k) != 65 || k[0] != 4 {
		t.Fatalf("public key: %x", k)
	}
	st, err := os.Stat(filepath.Join(dir, "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("push.json mode %v", st.Mode().Perm())
	}
	if _, err := s.Add(newBrowser(t).sub("u1", "https://fcm.googleapis.com/fcm/send/a")); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.PublicKey() != s.PublicKey() {
		t.Fatal("key changed across restarts")
	}
	if n := len(s2.ForUser("u1")); n != 1 {
		t.Fatalf("subscriptions after reopen: %d", n)
	}
}

func TestCheckEndpoint(t *testing.T) {
	for _, e := range []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/QK2…",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
		"https://FCM.googleapis.com:443/wp/abc",
	} {
		if err := checkEndpoint(e); err != nil {
			t.Errorf("%s refused", e)
		}
	}
	for _, e := range []string{
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com.evil.example/x",
		"https://evilfcm.googleapis.com.example/x",
		"https://notfcm.googleapis.com@127.0.0.1/x",
		"https://user@fcm.googleapis.com/x",
		"https://fcm.googleapis.com:8443/x",
		"https://127.0.0.1/x",
		"https://localhost:7878/api/state",
		"https://100.100.100.100/",
		"file:///etc/passwd",
		"",
	} {
		if err := checkEndpoint(e); err == nil {
			t.Errorf("%s accepted", e)
		}
	}
}

func TestAddValidatesAndReplaces(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	bad := b.sub("u1", "https://fcm.googleapis.com/fcm/send/a")
	bad.P256dh = b64.EncodeToString([]byte("not a key"))
	if _, err := s.Add(bad); err == nil {
		t.Error("invalid key accepted")
	}
	bad = b.sub("u1", "https://fcm.googleapis.com/fcm/send/a")
	bad.Auth = b64.EncodeToString([]byte("short"))
	if _, err := s.Add(bad); err == nil {
		t.Error("invalid auth secret accepted")
	}
	if _, err := s.Add(b.sub("u1", "https://127.0.0.1/a")); err == nil {
		t.Error("endpoint outside the push services accepted")
	}

	a, err := s.Add(b.sub("u1", "https://fcm.googleapis.com/fcm/send/a"))
	if err != nil {
		t.Fatal(err)
	}
	// The same browser, now signed in as someone else.
	a2, err := s.Add(b.sub("u2", "https://fcm.googleapis.com/fcm/send/a"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != a2.ID {
		t.Error("the id should follow the endpoint")
	}
	if len(s.ForUser("u1")) != 0 || len(s.ForUser("u2")) != 1 {
		t.Fatalf("endpoint should have moved to u2: u1=%d u2=%d", len(s.ForUser("u1")), len(s.ForUser("u2")))
	}
	if s.Remove("u1", a.Endpoint) {
		t.Error("u1 removed u2's registration")
	}
	if !s.Remove("u2", a.Endpoint) || len(s.ForUser("u2")) != 0 {
		t.Error("remove failed")
	}

	for i := 0; i < MaxPerUser+3; i++ {
		if _, err := s.Add(newBrowser(t).sub("u3", "https://fcm.googleapis.com/fcm/send/"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	list := s.ForUser("u3")
	if len(list) != MaxPerUser || !strings.HasSuffix(list[len(list)-1].Endpoint, string(rune('a'+MaxPerUser+2))) {
		t.Fatalf("kept %d, newest %s", len(list), list[len(list)-1].Endpoint)
	}
	s.RemoveUser("u3")
	if len(s.ForUser("u3")) != 0 {
		t.Fatal("RemoveUser kept registrations")
	}
}

func TestVAPIDToken(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	tok, err := s.vapidToken("https://web.push.apple.com/abc", "https://collie.example.ts.net", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	var hdr, claims map[string]any
	_ = json.Unmarshal(mustB64(t, parts[0]), &hdr)
	_ = json.Unmarshal(mustB64(t, parts[1]), &claims)
	if hdr["alg"] != "ES256" || claims["aud"] != "https://web.push.apple.com" || claims["sub"] != "https://collie.example.ts.net" ||
		claims["exp"] != float64(now.Add(12*time.Hour).Unix()) {
		t.Fatalf("header %v claims %v", hdr, claims)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature length %d", len(sig))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(s.key.Curve, mustB64(t, s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify with the public key")
	}
	tok, _ = s.vapidToken("https://fcm.googleapis.com/x", "", now)
	_ = json.Unmarshal(mustB64(t, strings.Split(tok, ".")[1]), &claims)
	if claims["sub"] != fallbackContact {
		t.Fatalf("fallback subject %v", claims["sub"])
	}
}

// pushService fakes a push service at an allowed host name.
func pushService(t *testing.T, s *Service, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	s.SetTransport(&http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})
}

func TestSend(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	sub, err := s.Add(b.sub("u1", "https://fcm.googleapis.com/fcm/send/abc"))
	if err != nil {
		t.Fatal(err)
	}
	var got *http.Request
	var body []byte
	status := http.StatusCreated
	pushService(t, s, func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "nope")
	})

	payload := []byte(`{"sid":"s1","status":"done"}`)
	if err := s.Send(context.Background(), sub, Message{Payload: payload, TTL: time.Hour, Urgency: "high", Topic: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/fcm/send/abc" || got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") != "3600" ||
		got.Header.Get("Urgency") != "high" || got.Header.Get("Topic") != "s1" {
		t.Fatalf("request %s %v", got.URL, got.Header)
	}
	if a := got.Header.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") || !strings.HasSuffix(a, ", k="+s.PublicKey()) {
		t.Fatalf("authorization %q", a)
	}
	if pt := b.decrypt(t, body); string(pt) != string(payload) {
		t.Fatalf("payload %q", pt)
	}

	status = http.StatusForbidden
	if err := s.Send(context.Background(), sub, Message{Payload: payload}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403: %v", err)
	}
	if len(s.ForUser("u1")) != 1 {
		t.Fatal("a refused push must not drop the registration")
	}
	status = http.StatusGone
	if err := s.Send(context.Background(), sub, Message{Payload: payload}); !errors.Is(err, ErrGone) {
		t.Fatalf("410: %v", err)
	}
	if len(s.ForUser("u1")) != 0 {
		t.Fatal("an expired registration must be dropped")
	}
	sub2, _ := s.Add(b.sub("u1", "https://fcm.googleapis.com/fcm/send/secret-token"))
	s.SetTransport(&http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}})
	if err := s.Send(context.Background(), sub2, Message{Payload: payload}); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("a network error must not reveal the endpoint's token: %v", err)
	}
	if err := s.Send(context.Background(), sub, Message{Payload: make([]byte, recordSize)}); !errors.Is(err, errPayloadTooBig) {
		t.Fatalf("oversized payload: %v", err)
	}
}
