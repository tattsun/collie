package auth

import (
	"encoding/json"
	"errors"
	"net/http"
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
	Kind    string // "login" | "register"
	User    User   // register: the user the passkey is for (may be new)
	NewUser bool   // register: create User on success
	Invite  string // register via invite: the invite secret
	Name    string // register: passkey name
	RP      RP
	data    webauthn.SessionData
	expires time.Time
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

func (p *Passkeys) put(c *Ceremony) string {
	id := newSecret()
	c.expires = time.Now().Add(5 * time.Minute)
	p.mu.Lock()
	for k, v := range p.cs {
		if time.Now().After(v.expires) {
			delete(p.cs, k)
		}
	}
	p.cs[id] = c
	p.mu.Unlock()
	return id
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
	b, err := json.Marshal(creation)
	return b, p.put(&c), err
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

// BeginLogin starts a username-less (discoverable) passkey sign-in.
func (p *Passkeys) BeginLogin(rp RP) (json.RawMessage, string, error) {
	w, err := p.wa(rp)
	if err != nil {
		return nil, "", err
	}
	assertion, data, err := w.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(assertion)
	return b, p.put(&Ceremony{Kind: "login", RP: rp, data: *data}), err
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

func friendly(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Details != "" {
		return errors.New("passkey check failed: " + pe.Details)
	}
	return errors.New("passkey check failed: " + err.Error())
}
