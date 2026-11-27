package auth

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// The server's periodic save must not put back what a CLI command (here
// `collie reset-passkeys --yes`) removed from auth.json meanwhile, while
// the uses it was saving still count for the sessions that remain.
func TestFlushKeepsCLIEdits(t *testing.T) {
	data := t.TempDir()
	srv, _ := Open(data)
	owner, _ := srv.EnsureBootstrapAdmin("admin")
	_ = srv.AddPasskey(owner, webauthn.Credential{ID: []byte("attackers-passkey")}, "")
	attacker, _ := srv.NewSession(owner, "attacker", "203.0.113.66")
	if _, _, ok := srv.SessionUser(attacker, "203.0.113.67"); !ok { // a pending last-seen change
		t.Fatal("session")
	}
	time.Sleep(20 * time.Millisecond)
	cli, _ := Open(data)
	if err := cli.ResetRP(); err != nil {
		t.Fatal(err)
	}
	srv.Flush()
	after, _ := Open(data)
	u, _ := after.User(owner)
	if _, _, valid := after.SessionUser(attacker, ""); len(u.Passkeys) != 0 || valid {
		t.Fatalf("after Flush: %d passkeys, attacker's session valid=%v", len(u.Passkeys), valid)
	}

	// A pending use of a session the CLI left alone is still written.
	keep, _ := srv.NewSession(owner, "phone", "198.51.100.1")
	if _, _, ok := srv.SessionUser(keep, "198.51.100.2"); !ok {
		t.Fatal("session")
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := cli.CreateInvite(Invite{Note: "from the CLI"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	srv.Flush()
	again, _ := Open(data)
	var ip string
	for _, x := range again.Sessions(owner) {
		if x.Hash == hashSecret(keep) {
			ip = x.LastIP
		}
	}
	if ip != "198.51.100.2" || len(again.Invites()) != 1 {
		t.Fatalf("last ip %q, %d invites", ip, len(again.Invites()))
	}
}

// One client can keep only a few sign-ins going (the oldest give way), a
// full table drops the oldest anonymous sign-in rather than refusing new
// ones, and checks of signed-in users have room of their own.
func TestCeremoniesAreBoundedPerClient(t *testing.T) {
	st, _ := Open(t.TempDir())
	p := NewPasskeys(st)
	put := func(key string, anon bool) string {
		id, err := p.put(&Ceremony{Kind: "login", key: key, anon: anon})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := put("addr:a", true)
	for i := 1; i < perKeyCeremonies; i++ {
		put("addr:a", true)
	}
	if _, ok := p.cs[first]; !ok {
		t.Fatal("dropped too early")
	}
	put("addr:a", true)
	if _, ok := p.cs[first]; ok {
		t.Fatal("the client's oldest sign-in was kept past its share")
	}
	for i := 0; len(p.cs) < maxAnonCeremonies; i++ {
		put(fmt.Sprintf("addr:%d", i), true)
	}
	put("addr:new", true)
	if len(p.cs) != maxAnonCeremonies {
		t.Fatalf("%d ceremonies", len(p.cs))
	}
	if id := put("user:u1", false); p.cs[id] == nil {
		t.Fatal("a signed-in user's check has no room")
	}
}
