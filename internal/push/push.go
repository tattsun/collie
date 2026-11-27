// Package push sends Web Push notifications (RFC 8030) to the browsers in
// which users turned notifications on, so they hear about their sessions
// while Collie is closed. Payloads are encrypted for the browser (RFC 8291:
// the push service cannot read them) and requests carry the server's VAPID
// signature (RFC 8292). The key, generated on first start, and the
// subscriptions live in <data>/push.json.
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Subscription is one browser's push endpoint, registered by a signed-in user.
type Subscription struct {
	// ID is derived from the endpoint. The page reports it over the
	// WebSocket, so pushes can skip a browser showing Collie right now.
	ID     string `json:"id"`
	UserID string `json:"userId"`
	// Session is the hash of the login session the browser registered
	// from ("" for the setup token / --no-auth): signing it out ends the
	// notifications.
	Session  string `json:"session,omitempty"`
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"` // the browser's public key (base64url)
	Auth     string `json:"auth"`   // the browser's authentication secret (base64url)
	// Contact is the VAPID subject: the address Collie is opened at.
	Contact   string `json:"contact,omitempty"`
	Lang      string `json:"lang,omitempty"` // language of the texts the service worker shows
	CreatedAt int64  `json:"createdAt"`
}

// Message is one push.
type Message struct {
	Payload []byte
	// TTL is how long the push service keeps the message for a browser
	// that is offline.
	TTL time.Duration
	// Urgency: "very-low", "low", "normal" or "high" (RFC 8030 §5.3).
	Urgency string
	// Topic, if set, replaces a still undelivered message with the same
	// topic (up to 32 base64url characters).
	Topic string
}

var (
	// ErrGone: the push service no longer knows the subscription (the
	// browser dropped it); it has been removed.
	ErrGone          = errors.New("the browser has dropped this device's registration for notifications; turn them off and on again")
	errEndpoint      = errors.New("this browser's push service is not supported")
	errSubscription  = errors.New("invalid push subscription")
	errPayloadTooBig = errors.New("push payload too large")
)

// MaxPerUser is how many browsers of one user are kept; registering one
// more drops the oldest.
const MaxPerUser = 20

// pushServices are the push services browsers use: Chrome and other
// Chromium browsers (FCM), Firefox, Safari (macOS and iOS) and Edge.
// Endpoints elsewhere are refused, so a user cannot make the server send
// requests to arbitrary addresses.
var pushServices = []string{
	"fcm.googleapis.com",
	"android.googleapis.com",
	"push.services.mozilla.com",
	"push.apple.com",
	"notify.windows.com",
}

func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return errEndpoint
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range pushServices {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return errEndpoint
}

var b64 = base64.RawURLEncoding

// decodeB64 accepts base64url with or without padding (browsers differ).
func decodeB64(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

func endpointID(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(h[:8])
}

type file struct {
	Key  string          `json:"vapidKey"` // P-256 private key (base64url scalar)
	Subs []*Subscription `json:"subscriptions"`
}

type Service struct {
	path   string
	key    *ecdsa.PrivateKey
	pub    string // the public key browsers subscribe with (base64url, uncompressed point)
	client *http.Client

	mu   sync.Mutex
	subs []*Subscription
}

// Open loads <dataDir>/push.json, creating it with a new VAPID key on
// first start.
func Open(dataDir string) (*Service, error) {
	s := &Service{
		path: filepath.Join(dataDir, "push.json"),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Push services answer directly; never follow a redirect elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	var f file
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if f.Key != "" {
		raw, err := decodeB64(f.Key)
		if err == nil {
			s.key, err = ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: VAPID key: %w", s.path, err)
		}
	} else {
		if s.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
	}
	pub, err := s.key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	s.pub = b64.EncodeToString(pub)
	for _, x := range f.Subs {
		if x != nil && x.ID != "" && x.UserID != "" {
			s.subs = append(s.subs, x)
		}
	}
	if f.Key == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetTransport replaces how requests reach the push services (tests).
func (s *Service) SetTransport(rt http.RoundTripper) { s.client.Transport = rt }

// PublicKey is the VAPID public key browsers subscribe with
// (applicationServerKey).
func (s *Service) PublicKey() string { return s.pub }

func (s *Service) saveLocked() error {
	raw, err := s.key.Bytes()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{Key: b64.EncodeToString(raw), Subs: s.subs}, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add registers a browser for sub.UserID, or refreshes its registration. A
// browser has one endpoint per site, so an endpoint registered before (by
// whoever was signed in on that browser then) moves to this user.
func (s *Service) Add(sub Subscription) (Subscription, error) {
	if err := checkEndpoint(sub.Endpoint); err != nil {
		return Subscription{}, err
	}
	if sub.UserID == "" {
		return Subscription{}, errSubscription
	}
	if k, err := decodeB64(sub.P256dh); err != nil {
		return Subscription{}, errSubscription
	} else if _, err := ecdh.P256().NewPublicKey(k); err != nil {
		return Subscription{}, errSubscription
	}
	if a, err := decodeB64(sub.Auth); err != nil || len(a) != 16 {
		return Subscription{}, errSubscription
	}
	sub.ID = endpointID(sub.Endpoint)
	sub.CreatedAt = time.Now().UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.subs[:0]
	var mine []*Subscription
	for _, x := range s.subs {
		if x.Endpoint == sub.Endpoint {
			continue
		}
		keep = append(keep, x)
		if x.UserID == sub.UserID {
			mine = append(mine, x)
		}
	}
	s.subs = keep
	for len(mine) >= MaxPerUser {
		s.dropLocked(mine[0].ID)
		mine = mine[1:]
	}
	c := sub
	s.subs = append(s.subs, &c)
	return sub, s.saveLocked()
}

func (s *Service) dropLocked(id string) bool {
	for i, x := range s.subs {
		if x.ID == id {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			return true
		}
	}
	return false
}

// Get returns the user's registration of a browser endpoint.
func (s *Service) Get(userID, endpoint string) (Subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.subs {
		if x.UserID == userID && x.Endpoint == endpoint {
			return *x, true
		}
	}
	return Subscription{}, false
}

// Remove forgets the user's registration of a browser endpoint.
func (s *Service) Remove(userID, endpoint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.subs {
		if x.UserID == userID && x.Endpoint == endpoint {
			s.dropLocked(x.ID)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// ForUser lists the browsers a user registered.
func (s *Service) ForUser(userID string) []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Subscription
	for _, x := range s.subs {
		if x.UserID == userID {
			out = append(out, *x)
		}
	}
	return out
}

// RemoveUser forgets all of a user's browsers.
func (s *Service) RemoveUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.subs[:0]
	for _, x := range s.subs {
		if x.UserID != userID {
			keep = append(keep, x)
		}
	}
	if len(keep) != len(s.subs) {
		s.subs = keep
		_ = s.saveLocked()
	} else {
		s.subs = keep
	}
}

// Send delivers m to one browser. A registration the push service no
// longer knows (404 / 410) is removed and ErrGone returned.
func (s *Service) Send(ctx context.Context, sub Subscription, m Message) error {
	if err := checkEndpoint(sub.Endpoint); err != nil {
		return err
	}
	body, err := encrypt(sub.P256dh, sub.Auth, m.Payload)
	if err != nil {
		return err
	}
	jwt, err := s.vapidToken(sub.Endpoint, sub.Contact, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.pub)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL/time.Second)))
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// Without the endpoint's path: it is the browser's push token.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("push service %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		s.Remove(sub.UserID, sub.Endpoint)
		return ErrGone
	}
	detail := strings.Join(strings.Fields(string(msg)), " ")
	return fmt.Errorf("push service %s answered %s %s", req.URL.Host, resp.Status, detail)
}

// fallbackContact is the VAPID subject when the address Collie is opened at
// is unknown (push services want a mailto: or https: URL).
const fallbackContact = "mailto:collie@localhost"

// vapidToken is the JWT identifying Collie to the push service (RFC 8292).
func (s *Service) vapidToken(endpoint, contact string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if contact == "" {
		contact = fallbackContact
	}
	claims, err := json.Marshal(map[string]any{
		"aud": "https://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(), // at most 24h
		"sub": contact,
	})
	if err != nil {
		return "", err
	}
	unsigned := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(unsigned))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return unsigned + "." + b64.EncodeToString(sig), nil
}

// recordSize is the aes128gcm record size: the whole payload is one record,
// so it holds at most maxPayload bytes (besides the delimiter and GCM tag).
const (
	recordSize = 4096
	maxPayload = recordSize - 1 - 16
)

// encrypt encodes a payload for the browser holding the subscription's
// keys (RFC 8291, aes128gcm content coding of RFC 8188).
func encrypt(p256dh, auth string, plaintext []byte) ([]byte, error) {
	ua, err := decodeB64(p256dh)
	if err != nil {
		return nil, errSubscription
	}
	secret, err := decodeB64(auth)
	if err != nil {
		return nil, errSubscription
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptWith(ua, secret, plaintext, salt, as)
}

func encryptWith(uaPublic, authSecret, plaintext, salt []byte, as *ecdh.PrivateKey) ([]byte, error) {
	if len(plaintext) > maxPayload {
		return nil, errPayloadTooBig
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, errSubscription
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	// Combine the shared secret with the browser's authentication secret
	// (RFC 8291 §3.3–3.4), then derive the content key and nonce (RFC 8188 §2.2).
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Header: salt, record size, key id (our ephemeral public key).
	out := binary.BigEndian.AppendUint32(slices.Clone(salt), recordSize)
	out = append(out, byte(len(asPublic)))
	out = append(out, asPublic...)
	// 0x02 marks the last record (appended to a copy: plaintext is the caller's).
	record := append(slices.Clip(plaintext), 0x02)
	return gcm.Seal(out, nonce, record, nil), nil
}
