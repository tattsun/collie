package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// waUser adapts User to the webauthn library.
type waUser struct{ u User }

func (w waUser) WebAuthnID() []byte          { return w.u.Handle }
func (w waUser) WebAuthnName() string        { return w.u.Name }
func (w waUser) WebAuthnDisplayName() string { return w.u.DisplayName }
func (w waUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(w.u.Passkeys))
	for _, p := range w.u.Passkeys {
		out = append(out, p.Cred)
	}
	return out
}

// RP identifies the relying party for one request.
type RP struct {
	ID     string // host name, e.g. box.tail1234.ts.net
	Origin string // e.g. https://box.tail1234.ts.net
}

// Ceremony state kept between the begin and finish steps.
type Ceremony struct {
	Kind    string // "login" | "register" | "verify"
	User    User   // register: the user the passkey is for (may be new); verify: the signed-in user
	NewUser bool   // register: create User on success
	Invite  string // register via invite: the invite secret
	Name    string // register: passkey name
	RP      RP
	data    webauthn.SessionData
	expires time.Time
	// key groups the ceremonies of one client (anonymous sign-ins, by
	// address) or one user or invite (the others).
	key  string
	anon bool
}

type Passkeys struct {
	st *Store
	mu sync.Mutex
	cs map[string]*Ceremony
}

func NewPasskeys(st *Store) *Passkeys {
	return &Passkeys{st: st, cs: map[string]*Ceremony{}}
}

func (p *Passkeys) wa(rp RP) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:          rp.ID,
		RPDisplayName: "Collie",
		RPOrigins:     []string{rp.Origin},
	})
}

// Ceremonies are kept in memory between their begin and finish steps.
// Anyone who can reach Collie can start a sign-in, so those are bounded per
// client address and in total, and the oldest give way to new ones rather
// than turning everyone away. Re-checks and new passkeys of signed-in users
// and invitations are kept apart, so a flood of sign-ins never blocks them.
const (
	maxAnonCeremonies    = 2000
	maxTrustedCeremonies = 2000
	perKeyCeremonies     = 8
)

// ErrBusy means too many sign-in attempts are in progress.
var ErrBusy = errors.New("too many sign-in attempts right now; try again in a minute")

func (p *Passkeys) put(c *Ceremony) (string, error) {
	id := newSecret()
	t := time.Now()
	c.expires = t.Add(5 * time.Minute)
	p.mu.Lock()
	defer p.mu.Unlock()
	var mine, pool []string
	for k, v := range p.cs {
		switch {
		case t.After(v.expires):
			delete(p.cs, k)
		case v.anon == c.anon:
			pool = append(pool, k)
			if v.key == c.key {
				mine = append(mine, k)
			}
		}
	}
	if len(mine) >= perKeyCeremonies {
		k := p.oldest(mine)
		delete(p.cs, k)
		pool = slices.DeleteFunc(pool, func(x string) bool { return x == k })
	}
	if c.anon {
		if len(pool) >= maxAnonCeremonies {
			delete(p.cs, p.oldest(pool))
		}
	} else if len(pool) >= maxTrustedCeremonies {
		return "", ErrBusy
	}
	p.cs[id] = c
	return id, nil
}

// oldest returns the ceremony among ids that expires first; p.mu must be
// held.
func (p *Passkeys) oldest(ids []string) string {
	best := ids[0]
	for _, k := range ids[1:] {
		if p.cs[k].expires.Before(p.cs[best].expires) {
			best = k
		}
	}
	return best
}

func (p *Passkeys) take(id, kind string) (*Ceremony, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.cs[id]
	delete(p.cs, id)
	if !ok || c.Kind != kind || time.Now().After(c.expires) {
		return nil, errors.New("the sign-in attempt expired; try again")
	}
	return c, nil
}

// BeginRegistration starts adding a passkey for u (an existing user, or a
// new one created when the ceremony completes).
func (p *Passkeys) BeginRegistration(rp RP, c Ceremony) (json.RawMessage, string, error) {
	w, err := p.wa(rp)
	if err != nil {
		return nil, "", err
	}
	var exclude []protocol.CredentialDescriptor
	for _, k := range c.User.Passkeys {
		exclude = append(exclude, k.Cred.Descriptor())
	}
	creation, data, err := w.BeginRegistration(waUser{c.User},
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude),
	)
	if err != nil {
		return nil, "", err
	}
	c.Kind, c.RP, c.data = "register", rp, *data
	c.key = "user:" + c.User.ID
	if c.Invite != "" {
		c.key = "invite:" + hashSecret(c.Invite)
	}
	return finishBegin(creation, &c, p)
}

func finishBegin(options any, c *Ceremony, p *Passkeys) (json.RawMessage, string, error) {
	b, err := json.Marshal(options)
	if err != nil {
		return nil, "", err
	}
	id, err := p.put(c)
	return b, id, err
}

// FinishRegistration verifies the authenticator's response. The caller
// persists the credential (and creates the user if needed).
func (p *Passkeys) FinishRegistration(id string, r *http.Request) (*Ceremony, *webauthn.Credential, error) {
	c, err := p.take(id, "register")
	if err != nil {
		return nil, nil, err
	}
	w, err := p.wa(c.RP)
	if err != nil {
		return nil, nil, err
	}
	cred, err := w.FinishRegistration(waUser{c.User}, c.data, r)
	if err != nil {
		return nil, nil, friendly(err)
	}
	return c, cred, nil
}

// BeginLogin starts a username-less (discoverable) passkey sign-in for a
// client (its address, as rate limited).
func (p *Passkeys) BeginLogin(rp RP, client string) (json.RawMessage, string, error) {
	w, err := p.wa(rp)
	if err != nil {
		return nil, "", err
	}
	assertion, data, err := w.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	return finishBegin(assertion, &Ceremony{Kind: "login", RP: rp, data: *data, key: "addr:" + client, anon: true}, p)
}

// FinishLogin verifies a sign-in and returns the user.
func (p *Passkeys) FinishLogin(id string, r *http.Request) (User, error) {
	c, err := p.take(id, "login")
	if err != nil {
		return User{}, err
	}
	w, err := p.wa(c.RP)
	if err != nil {
		return User{}, err
	}
	var found User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, ok := p.st.UserByHandle(userHandle)
		if !ok || u.Disabled {
			return nil, errors.New("unknown passkey")
		}
		found = u
		return waUser{u}, nil
	}
	_, cred, err := w.FinishPasskeyLogin(handler, c.data, r)
	if err != nil {
		return User{}, friendly(err)
	}
	if cred.Authenticator.CloneWarning {
		return User{}, errors.New("this passkey looks cloned (signature counter went backwards); sign-in refused")
	}
	p.st.PasskeyUsed(found.ID, *cred)
	return found, nil
}

// BeginVerify asks the signed-in user to confirm it's them with one of
// their own passkeys (before a sensitive change).
func (p *Passkeys) BeginVerify(rp RP, u User) (json.RawMessage, string, error) {
	if len(u.Passkeys) == 0 {
		return nil, "", errors.New("you have no passkey to confirm with")
	}
	w, err := p.wa(rp)
	if err != nil {
		return nil, "", err
	}
	assertion, data, err := w.BeginLogin(waUser{u}, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	return finishBegin(assertion, &Ceremony{Kind: "verify", RP: rp, User: u, data: *data, key: "user:" + u.ID}, p)
}

// FinishVerify checks that the response comes from one of uid's passkeys.
func (p *Passkeys) FinishVerify(id, uid string, r *http.Request) error {
	c, err := p.take(id, "verify")
	if err != nil {
		return err
	}
	if c.User.ID != uid {
		return errors.New("mismatched request")
	}
	u, ok := p.st.User(uid)
	if !ok || u.Disabled {
		return errors.New("unknown user")
	}
	w, err := p.wa(c.RP)
	if err != nil {
		return err
	}
	cred, err := w.FinishLogin(waUser{u}, c.data, r)
	if err != nil {
		return friendly(err)
	}
	if cred.Authenticator.CloneWarning {
		return errors.New("this passkey looks cloned (signature counter went backwards); refused")
	}
	p.st.PasskeyUsed(u.ID, *cred)
	return nil
}

func friendly(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Details != "" {
		return errors.New("passkey check failed: " + pe.Details)
	}
	return errors.New("passkey check failed: " + err.Error())
}
