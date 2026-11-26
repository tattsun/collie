// Package auth holds Collie's users, their passkeys, login sessions,
// invitations and groups, persisted in <data>/auth.json.
//
// The file is also edited by CLI subcommands (e.g. issuing a recovery link)
// while the server runs, so writes take an exclusive file lock and the
// server reloads the file whenever it changed on disk.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	SessionTTL = 30 * 24 * time.Hour
	InviteTTL  = 7 * 24 * time.Hour
	LinkTTL    = 30 * time.Minute // recovery / add-device links
)

type Passkey struct {
	ID        string              `json:"id"` // base64url credential id
	Name      string              `json:"name"`
	CreatedAt int64               `json:"createdAt"`
	LastUsed  int64               `json:"lastUsed,omitempty"`
	Cred      webauthn.Credential `json:"cred"`
}

type User struct {
	ID          string    `json:"id"`
	Handle      []byte    `json:"handle"` // WebAuthn user handle (random, opaque)
	Name        string    `json:"name"`   // login name; also the user's project folder
	DisplayName string    `json:"displayName"`
	Admin       bool      `json:"admin"`
	Disabled    bool      `json:"disabled,omitempty"`
	CreatedAt   int64     `json:"createdAt"`
	LastSeen    int64     `json:"lastSeen,omitempty"`
	Passkeys    []Passkey `json:"passkeys,omitempty"`
}

type Session struct {
	Hash      string `json:"hash"` // sha256 of the cookie value
	UserID    string `json:"userId"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	LastSeen  int64  `json:"lastSeen"`
	Agent     string `json:"agent,omitempty"`
}

// Invite is a one-time link. With UserID set it adds a passkey to that
// existing user (new device / recovery); otherwise it creates a new user.
type Invite struct {
	Hash      string `json:"hash"`
	Admin     bool   `json:"admin,omitempty"`
	UserID    string `json:"userId,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Note      string `json:"note,omitempty"`
}

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

type Group struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Members   map[string]string `json:"members"` // user id -> role
	CreatedAt int64             `json:"createdAt"`
}

type data struct {
	Users    []*User    `json:"users"`
	Sessions []*Session `json:"sessions"`
	Invites  []*Invite  `json:"invites"`
	Groups   []*Group   `json:"groups"`
	// The relying party (host name) passkeys are bound to; set by the
	// first registration.
	RPID   string `json:"rpId,omitempty"`
	Origin string `json:"origin,omitempty"`
}

type Store struct {
	path string

	mu    sync.Mutex
	d     data
	mtime time.Time
	dirty bool // only LastSeen-style changes pending
	// OnChange is called after users / groups change (not on session churn).
	OnChange func()
}

var (
	ErrNotFound = errors.New("not found")
	nameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "auth.json")}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

// ---- persistence ----

func (s *Store) lockFile() (func(), error) {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// reloadLocked re-reads the file if it changed on disk (or always, with force).
func (s *Store) reloadLocked(force bool) error {
	st, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force && st.ModTime().Equal(s.mtime) {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var d data
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("auth.json: %w", err)
	}
	s.d, s.mtime = d, st.ModTime()
	return nil
}

// saveLocked writes the file; s.mu must be held. It takes the file lock.
func (s *Store) saveLocked() error {
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	return s.writeLocked()
}

// writeLocked writes the file; s.mu and the file lock must be held.
func (s *Store) writeLocked() error {
	b, err := json.MarshalIndent(s.d, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.mtime = st.ModTime()
	}
	s.dirty = false
	return nil
}

// update applies fn to fresh data and persists it, holding the file lock
// throughout so concurrent CLI edits are never lost.
func (s *Store) update(fn func(d *data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadLocked(false); err != nil {
		return err
	}
	if err := fn(&s.d); err != nil {
		return err
	}
	return s.writeLocked()
}

func (s *Store) read() *data {
	s.mu.Lock()
	_ = s.reloadLocked(false)
	return &s.d
}

func (s *Store) changed() {
	if s.OnChange != nil {
		go s.OnChange()
	}
}

// Flush persists pending last-seen updates.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		_ = s.saveLocked()
	}
}

// ---- helpers ----

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func newID() string { return hex.EncodeToString(randomBytes(8)) }

func newSecret() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }

func hashSecret(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func now() int64 { return time.Now().UnixMilli() }

func (d *data) user(id string) *User {
	for _, u := range d.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (d *data) group(id string) *Group {
	for _, g := range d.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func cloneUser(u *User) User {
	c := *u
	c.Passkeys = append([]Passkey(nil), u.Passkeys...)
	c.Handle = append([]byte(nil), u.Handle...)
	return c
}

func cloneGroup(g *Group) Group {
	c := *g
	c.Members = make(map[string]string, len(g.Members))
	for k, v := range g.Members {
		c.Members[k] = v
	}
	return c
}

// reserved names would collide with Collie's folders under the project root
var reserved = map[string]bool{"groups": true, "users": true}

// ValidName checks a user or group name (also used as a folder name).
func ValidName(n string) bool { return nameRe.MatchString(n) && !reserved[n] }

// ---- users ----

// EnsureBootstrapAdmin makes sure an admin exists, creating one named
// `name` (without passkeys) on first start. It returns that admin's id;
// pre-existing projects are assigned to it.
func (s *Store) EnsureBootstrapAdmin(name string) (string, error) {
	var id string
	err := s.update(func(d *data) error {
		for _, u := range d.Users {
			if u.Admin && !u.Disabled {
				id = u.ID
				return nil
			}
		}
		if len(d.Users) > 0 {
			id = d.Users[0].ID
			d.Users[0].Admin = true
			return nil
		}
		name = sanitizeName(name)
		u := &User{ID: newID(), Handle: randomBytes(32), Name: name, DisplayName: name, Admin: true, CreatedAt: now()}
		d.Users = append(d.Users, u)
		id = u.ID
		return nil
	})
	return id, err
}

func sanitizeName(n string) string {
	n = strings.ToLower(n)
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(b.String(), "-_")
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" || reserved[out] {
		out = "admin"
	}
	return out
}

// PasskeysExist reports whether anyone has registered a passkey; until then
// the legacy token / Tailscale sign-in is accepted (bootstrap).
func (s *Store) PasskeysExist() bool {
	d := s.read()
	defer s.mu.Unlock()
	for _, u := range d.Users {
		if len(u.Passkeys) > 0 && !u.Disabled {
			return true
		}
	}
	return false
}

func (s *Store) User(id string) (User, bool) {
	d := s.read()
	defer s.mu.Unlock()
	if u := d.user(id); u != nil {
		return cloneUser(u), true
	}
	return User{}, false
}

func (s *Store) UserByHandle(h []byte) (User, bool) {
	d := s.read()
	defer s.mu.Unlock()
	for _, u := range d.Users {
		if string(u.Handle) == string(h) {
			return cloneUser(u), true
		}
	}
	return User{}, false
}

func (s *Store) UserByName(name string) (User, bool) {
	d := s.read()
	defer s.mu.Unlock()
	for _, u := range d.Users {
		if u.Name == name {
			return cloneUser(u), true
		}
	}
	return User{}, false
}

func (s *Store) Users() []User {
	d := s.read()
	defer s.mu.Unlock()
	out := make([]User, 0, len(d.Users))
	for _, u := range d.Users {
		out = append(out, cloneUser(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// NewHandle returns a fresh WebAuthn user handle.
func NewHandle() []byte { return randomBytes(32) }

func (s *Store) CreateUser(name, display string, admin bool) (User, error) {
	return s.CreateUserWithHandle(name, display, admin, nil)
}

// CreateUserWithHandle creates a user whose WebAuthn handle was already
// used in a registration ceremony.
func (s *Store) CreateUserWithHandle(name, display string, admin bool, handle []byte) (User, error) {
	if len(handle) == 0 {
		handle = randomBytes(32)
	}
	var nu User
	if !ValidName(name) {
		return nu, errors.New("user name: 1-32 of a-z 0-9 - _ (starting with a letter or digit)")
	}
	if strings.TrimSpace(display) == "" {
		display = name
	}
	err := s.update(func(d *data) error {
		for _, u := range d.Users {
			if u.Name == name {
				return fmt.Errorf("the name %q is taken", name)
			}
		}
		u := &User{ID: newID(), Handle: handle, Name: name, DisplayName: strings.TrimSpace(display), Admin: admin, CreatedAt: now()}
		d.Users = append(d.Users, u)
		nu = cloneUser(u)
		return nil
	})
	if err == nil {
		s.changed()
	}
	return nu, err
}

func activeAdmins(d *data, except string) int {
	n := 0
	for _, u := range d.Users {
		if u.ID != except && u.Admin && !u.Disabled {
			n++
		}
	}
	return n
}

// UpdateUser changes a user's display name, admin flag or disabled state.
// At least one active admin always remains.
func (s *Store) UpdateUser(id string, display *string, admin, disabled *bool) (User, error) {
	var out User
	err := s.update(func(d *data) error {
		u := d.user(id)
		if u == nil {
			return ErrNotFound
		}
		if (admin != nil && !*admin || disabled != nil && *disabled) && u.Admin && activeAdmins(d, id) == 0 {
			return errors.New("at least one active admin must remain")
		}
		if display != nil && strings.TrimSpace(*display) != "" {
			u.DisplayName = strings.TrimSpace(*display)
		}
		if admin != nil {
			u.Admin = *admin
		}
		if disabled != nil {
			u.Disabled = *disabled
			if u.Disabled {
				d.Sessions = dropSessions(d.Sessions, id, "")
			}
		}
		out = cloneUser(u)
		return nil
	})
	if err == nil {
		s.changed()
	}
	return out, err
}

// DeleteUser removes the user, their sessions and group memberships.
func (s *Store) DeleteUser(id string) error {
	err := s.update(func(d *data) error {
		u := d.user(id)
		if u == nil {
			return ErrNotFound
		}
		if u.Admin && activeAdmins(d, id) == 0 {
			return errors.New("at least one active admin must remain")
		}
		users := d.Users[:0]
		for _, x := range d.Users {
			if x.ID != id {
				users = append(users, x)
			}
		}
		d.Users = users
		d.Sessions = dropSessions(d.Sessions, id, "")
		for _, g := range d.Groups {
			delete(g.Members, id)
		}
		return nil
	})
	if err == nil {
		s.changed()
	}
	return err
}

// ---- passkeys ----

func credID(c *webauthn.Credential) string { return base64.RawURLEncoding.EncodeToString(c.ID) }

func (s *Store) AddPasskey(userID string, c webauthn.Credential, name string) error {
	err := s.update(func(d *data) error {
		u := d.user(userID)
		if u == nil {
			return ErrNotFound
		}
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("Passkey %d", len(u.Passkeys)+1)
		}
		u.Passkeys = append(u.Passkeys, Passkey{ID: credID(&c), Name: strings.TrimSpace(name), CreatedAt: now(), Cred: c})
		return nil
	})
	if err == nil {
		s.changed()
	}
	return err
}

// PasskeyUsed records a successful login with the credential.
func (s *Store) PasskeyUsed(userID string, c webauthn.Credential) {
	_ = s.update(func(d *data) error {
		u := d.user(userID)
		if u == nil {
			return nil
		}
		for i := range u.Passkeys {
			if u.Passkeys[i].ID == credID(&c) {
				u.Passkeys[i].Cred.Authenticator = c.Authenticator
				u.Passkeys[i].Cred.Flags = c.Flags
				u.Passkeys[i].LastUsed = now()
			}
		}
		u.LastSeen = now()
		return nil
	})
}

func (s *Store) RenamePasskey(userID, pkID, name string) error {
	return s.update(func(d *data) error {
		u := d.user(userID)
		if u == nil {
			return ErrNotFound
		}
		for i := range u.Passkeys {
			if u.Passkeys[i].ID == pkID {
				u.Passkeys[i].Name = strings.TrimSpace(name)
				return nil
			}
		}
		return ErrNotFound
	})
}

// RemovePasskey deletes one passkey. Removing the last passkey of the last
// admin who has one is refused (it would lock everyone out).
func (s *Store) RemovePasskey(userID, pkID string) error {
	err := s.update(func(d *data) error {
		u := d.user(userID)
		if u == nil {
			return ErrNotFound
		}
		var keep []Passkey
		for _, p := range u.Passkeys {
			if p.ID != pkID {
				keep = append(keep, p)
			}
		}
		if len(keep) == len(u.Passkeys) {
			return ErrNotFound
		}
		if len(keep) == 0 && u.Admin {
			others := 0
			for _, x := range d.Users {
				if x.ID != u.ID && x.Admin && !x.Disabled && len(x.Passkeys) > 0 {
					others++
				}
			}
			if others == 0 {
				return errors.New("this is the last passkey of the last admin; add another one first")
			}
		}
		u.Passkeys = keep
		return nil
	})
	if err == nil {
		s.changed()
	}
	return err
}

// ---- relying party ----

func (s *Store) RP() (rpID, origin string) {
	d := s.read()
	defer s.mu.Unlock()
	return d.RPID, d.Origin
}

func (s *Store) SetRP(rpID, origin string) error {
	return s.update(func(d *data) error {
		if d.RPID == "" {
			d.RPID, d.Origin = rpID, origin
		}
		return nil
	})
}

// ResetRP forgets the bound host name (all passkeys become unusable).
func (s *Store) ResetRP() error {
	return s.update(func(d *data) error {
		d.RPID, d.Origin = "", ""
		for _, u := range d.Users {
			u.Passkeys = nil
		}
		d.Sessions = nil
		return nil
	})
}

// ---- login sessions ----

// NewSession returns the cookie value for a fresh login session.
func (s *Store) NewSession(userID, agent string) (string, error) {
	secret := newSecret()
	err := s.update(func(d *data) error {
		t := now()
		// drop expired sessions while we are here
		keep := d.Sessions[:0]
		for _, x := range d.Sessions {
			if x.ExpiresAt > t {
				keep = append(keep, x)
			}
		}
		d.Sessions = append(keep, &Session{Hash: hashSecret(secret), UserID: userID, CreatedAt: t,
			ExpiresAt: t + SessionTTL.Milliseconds(), LastSeen: t, Agent: agent})
		if u := d.user(userID); u != nil {
			u.LastSeen = t
		}
		return nil
	})
	return secret, err
}

// SessionUser resolves a session cookie. Sessions slide: each use (at most
// hourly) pushes the expiry SessionTTL into the future.
func (s *Store) SessionUser(secret string) (User, string, bool) {
	if secret == "" {
		return User{}, "", false
	}
	h := hashSecret(secret)
	d := s.read()
	defer s.mu.Unlock()
	t := now()
	for _, x := range d.Sessions {
		if x.Hash != h || x.ExpiresAt <= t {
			continue
		}
		u := d.user(x.UserID)
		if u == nil || u.Disabled {
			return User{}, "", false
		}
		if t-x.LastSeen > time.Hour.Milliseconds() {
			x.LastSeen = t
			x.ExpiresAt = t + SessionTTL.Milliseconds()
			u.LastSeen = t
			s.dirty = true
		}
		return cloneUser(u), h, true
	}
	return User{}, "", false
}

// SessionValid reports whether the session (by hash) still exists.
func (s *Store) SessionValid(hash string) bool {
	d := s.read()
	defer s.mu.Unlock()
	t := now()
	for _, x := range d.Sessions {
		if x.Hash == hash && x.ExpiresAt > t {
			u := d.user(x.UserID)
			return u != nil && !u.Disabled
		}
	}
	return false
}

func (s *Store) Sessions(userID string) []Session {
	d := s.read()
	defer s.mu.Unlock()
	var out []Session
	t := now()
	for _, x := range d.Sessions {
		if x.UserID == userID && x.ExpiresAt > t {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

func dropSessions(list []*Session, userID, exceptHash string) []*Session {
	keep := list[:0]
	for _, x := range list {
		if x.UserID != userID || (exceptHash != "" && x.Hash == exceptHash) {
			keep = append(keep, x)
		}
	}
	return keep
}

func (s *Store) DeleteSession(hash string) {
	_ = s.update(func(d *data) error {
		keep := d.Sessions[:0]
		for _, x := range d.Sessions {
			if x.Hash != hash {
				keep = append(keep, x)
			}
		}
		d.Sessions = keep
		return nil
	})
}

// DeleteUserSessions signs the user out everywhere except exceptHash.
func (s *Store) DeleteUserSessions(userID, exceptHash string) {
	_ = s.update(func(d *data) error {
		d.Sessions = dropSessions(d.Sessions, userID, exceptHash)
		return nil
	})
}

// ---- invitations ----

func (s *Store) CreateInvite(inv Invite, ttl time.Duration) (string, error) {
	secret := newSecret()
	err := s.update(func(d *data) error {
		if inv.UserID != "" && d.user(inv.UserID) == nil {
			return ErrNotFound
		}
		t := now()
		keep := d.Invites[:0]
		for _, x := range d.Invites {
			if x.ExpiresAt > t {
				keep = append(keep, x)
			}
		}
		inv.Hash = hashSecret(secret)
		inv.CreatedAt = t
		inv.ExpiresAt = t + ttl.Milliseconds()
		d.Invites = append(keep, &inv)
		return nil
	})
	return secret, err
}

func (s *Store) Invite(secret string) (Invite, bool) {
	h := hashSecret(secret)
	d := s.read()
	defer s.mu.Unlock()
	for _, x := range d.Invites {
		if x.Hash == h && x.ExpiresAt > now() {
			return *x, true
		}
	}
	return Invite{}, false
}

// ConsumeInvite removes and returns a valid invite.
func (s *Store) ConsumeInvite(secret string) (Invite, error) {
	h := hashSecret(secret)
	var out Invite
	err := s.update(func(d *data) error {
		for i, x := range d.Invites {
			if x.Hash == h {
				d.Invites = append(d.Invites[:i], d.Invites[i+1:]...)
				if x.ExpiresAt <= now() {
					return errors.New("this link has expired")
				}
				out = *x
				return nil
			}
		}
		return errors.New("this link is invalid or was already used")
	})
	return out, err
}

func (s *Store) Invites() []Invite {
	d := s.read()
	defer s.mu.Unlock()
	var out []Invite
	for _, x := range d.Invites {
		if x.ExpiresAt > now() {
			out = append(out, *x)
		}
	}
	return out
}

func (s *Store) RevokeInvite(hash string) error {
	return s.update(func(d *data) error {
		for i, x := range d.Invites {
			if x.Hash == hash {
				d.Invites = append(d.Invites[:i], d.Invites[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// ---- groups ----

func (s *Store) Groups() []Group {
	d := s.read()
	defer s.mu.Unlock()
	out := make([]Group, 0, len(d.Groups))
	for _, g := range d.Groups {
		out = append(out, cloneGroup(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Group(id string) (Group, bool) {
	d := s.read()
	defer s.mu.Unlock()
	if g := d.group(id); g != nil {
		return cloneGroup(g), true
	}
	return Group{}, false
}

// Role returns the user's role in the group ("" if not a member).
func (s *Store) Role(groupID, userID string) string {
	d := s.read()
	defer s.mu.Unlock()
	if g := d.group(groupID); g != nil {
		return g.Members[userID]
	}
	return ""
}

func (s *Store) CreateGroup(name, ownerID string) (Group, error) {
	var out Group
	if !ValidName(name) {
		return out, errors.New("group name: 1-32 of a-z 0-9 - _")
	}
	err := s.update(func(d *data) error {
		for _, g := range d.Groups {
			if g.Name == name {
				return fmt.Errorf("the group %q exists", name)
			}
		}
		g := &Group{ID: newID(), Name: name, Members: map[string]string{ownerID: RoleOwner}, CreatedAt: now()}
		d.Groups = append(d.Groups, g)
		out = cloneGroup(g)
		return nil
	})
	if err == nil {
		s.changed()
	}
	return out, err
}

// SetMember adds/updates (role != "") or removes (role == "") a member.
// A group always keeps at least one owner.
func (s *Store) SetMember(groupID, userID, role string) error {
	err := s.update(func(d *data) error {
		g := d.group(groupID)
		if g == nil {
			return ErrNotFound
		}
		if d.user(userID) == nil {
			return errors.New("no such user")
		}
		if role != "" && role != RoleOwner && role != RoleMember {
			return errors.New("role must be owner or member")
		}
		if g.Members[userID] == RoleOwner && role != RoleOwner {
			owners := 0
			for _, r := range g.Members {
				if r == RoleOwner {
					owners++
				}
			}
			if owners <= 1 {
				return errors.New("a group needs at least one owner")
			}
		}
		if role == "" {
			delete(g.Members, userID)
		} else {
			g.Members[userID] = role
		}
		return nil
	})
	if err == nil {
		s.changed()
	}
	return err
}

func (s *Store) DeleteGroup(id string) error {
	err := s.update(func(d *data) error {
		for i, g := range d.Groups {
			if g.ID == id {
				d.Groups = append(d.Groups[:i], d.Groups[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
	if err == nil {
		s.changed()
	}
	return err
}

// GroupsOf returns the ids of the groups the user belongs to.
func (s *Store) GroupsOf(userID string) map[string]string {
	d := s.read()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, g := range d.Groups {
		if r, ok := g.Members[userID]; ok {
			out[g.ID] = r
		}
	}
	return out
}

// IsAdmin implements session.Access.
func (s *Store) IsAdmin(id string) bool {
	u, ok := s.User(id)
	return ok && u.Admin && !u.Disabled
}

// GroupName implements session.Access.
func (s *Store) GroupName(id string) string {
	if g, ok := s.Group(id); ok {
		return g.Name
	}
	return id
}
