package auth

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestBootstrapAndAdmins(t *testing.T) {
	s, dir := open(t)
	owner, err := s.EnsureBootstrapAdmin("Tattsun")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.EnsureBootstrapAdmin("someone")
	if again != owner {
		t.Fatal("bootstrap admin must be created once")
	}
	u, _ := s.User(owner)
	if !u.Admin || u.Name != "tattsun" {
		t.Fatalf("owner = %+v", u)
	}
	if s.PasskeysExist() {
		t.Fatal("no passkeys yet")
	}
	no := false
	if _, err := s.UpdateUser(owner, nil, &no, nil); err == nil {
		t.Fatal("the last admin must not be demoted")
	}
	bob, err := s.CreateUser("bob", "Bob", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("bob", "", false); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.CreateUser("Bad Name", "", false); err == nil {
		t.Fatal("invalid name accepted")
	}
	for _, r := range []string{"groups", "users"} {
		if _, err := s.CreateUser(r, "", false); err == nil {
			t.Fatalf("reserved name %q accepted", r)
		}
		if _, err := s.CreateGroup(r, owner); err == nil {
			t.Fatalf("reserved group name %q accepted", r)
		}
	}
	if _, err := s.UpdateUser(owner, nil, &no, nil); err != nil {
		t.Fatalf("demote with another admin left: %v", err)
	}
	if err := s.DeleteUser(bob.ID); err == nil {
		t.Fatal("deleting the last admin accepted")
	}
	// persisted and reloaded by another process (CLI)
	s2, _ := Open(dir)
	if u, ok := s2.UserByName("bob"); !ok || !u.Admin {
		t.Fatal("not persisted")
	}
}

func TestPasskeysAndSessions(t *testing.T) {
	s, _ := open(t)
	owner, _ := s.EnsureBootstrapAdmin("admin")
	if err := s.AddPasskey(owner, webauthn.Credential{ID: []byte("cred-1")}, ""); err != nil {
		t.Fatal(err)
	}
	if !s.PasskeysExist() {
		t.Fatal("passkey not recorded")
	}
	u, _ := s.User(owner)
	if err := s.RemovePasskey(owner, u.Passkeys[0].ID); err == nil {
		t.Fatal("removing the last admin's last passkey must be refused")
	}
	secret, err := s.NewSession(owner, "test", "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	got, sess, ok := s.SessionUser(secret, "203.0.113.5")
	hash := sess.Hash
	if !ok || got.ID != owner || !s.SessionValid(hash) {
		t.Fatal("session not resolved")
	}
	// Signing in is a passkey check.
	if !sess.Verified() || sess.IP != "203.0.113.5" {
		t.Fatalf("new session: %+v", sess)
	}
	if _, _, ok := s.SessionUser(secret+"x", ""); ok {
		t.Fatal("wrong secret accepted")
	}
	s.DeleteSession(hash)
	if _, _, ok := s.SessionUser(secret, ""); ok || s.SessionValid(hash) {
		t.Fatal("deleted session still valid")
	}
	// disabling a user ends their sessions
	member, _ := s.CreateUser("m", "", false)
	sec2, _ := s.NewSession(member.ID, "x", "")
	yes := true
	s.UpdateUser(member.ID, nil, nil, &yes)
	if _, _, ok := s.SessionUser(sec2, ""); ok {
		t.Fatal("disabled user still signed in")
	}
}

func TestSessionVerificationAndDevices(t *testing.T) {
	s, _ := open(t)
	owner, _ := s.EnsureBootstrapAdmin("admin")
	other, _ := s.CreateUser("other", "", false)
	sec, _ := s.NewSession(owner, "Mac", "198.51.100.1")
	sec2, _ := s.NewSession(owner, "iPhone", "198.51.100.2")
	osec, _ := s.NewSession(other.ID, "x", "")

	// The passkey check expires; re-verifying renews it.
	s.update(func(d *data) error {
		for _, x := range d.Sessions {
			x.VerifiedAt = now() - VerifyTTL.Milliseconds() - 1
		}
		return nil
	})
	_, sess, _ := s.SessionUser(sec, "198.51.100.9")
	if sess.Verified() {
		t.Fatal("stale passkey check counted as recent")
	}
	if err := s.MarkVerified(sess.Hash); err != nil {
		t.Fatal(err)
	}
	if _, sess, _ = s.SessionUser(sec, "198.51.100.9"); !sess.Verified() || sess.LastIP != "198.51.100.9" || sess.IP != "198.51.100.1" {
		t.Fatalf("after re-verification: %+v", sess)
	}

	// Signing one device out leaves the others; nobody else's devices can be targeted.
	_, s2, _ := s.SessionUser(sec2, "")
	_, os, _ := s.SessionUser(osec, "")
	if _, err := s.DeleteUserSession(owner, os.ID()); err == nil {
		t.Fatal("signed out another user's device")
	}
	if _, err := s.DeleteUserSession(owner, ""); err == nil {
		t.Fatal("empty id matched a session")
	}
	h, err := s.DeleteUserSession(owner, s2.ID())
	if err != nil || h != s2.Hash {
		t.Fatalf("delete: %q %v", h, err)
	}
	if _, _, ok := s.SessionUser(sec2, ""); ok {
		t.Fatal("signed-out device still signed in")
	}
	if _, _, ok := s.SessionUser(sec, ""); !ok {
		t.Fatal("the other device was signed out too")
	}
	if _, _, ok := s.SessionUser(osec, ""); !ok {
		t.Fatal("another user's device was signed out")
	}
}

func TestInvites(t *testing.T) {
	s, _ := open(t)
	owner, _ := s.EnsureBootstrapAdmin("admin")
	code, err := s.CreateInvite(Invite{CreatedBy: owner}, InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Invite(code); !ok {
		t.Fatal("invite not found")
	}
	if _, err := s.ConsumeInvite(code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeInvite(code); err == nil {
		t.Fatal("invite used twice")
	}
	short, _ := s.CreateInvite(Invite{CreatedBy: owner}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Invite(short); ok {
		t.Fatal("expired invite still valid")
	}
	if _, err := s.CreateInvite(Invite{UserID: "nope"}, LinkTTL); err == nil {
		t.Fatal("device link for unknown user accepted")
	}
}

func TestGroups(t *testing.T) {
	s, _ := open(t)
	owner, _ := s.EnsureBootstrapAdmin("admin")
	m, _ := s.CreateUser("member", "", false)
	g, err := s.CreateGroup("team", owner)
	if err != nil {
		t.Fatal(err)
	}
	if s.Role(g.ID, owner) != RoleOwner || s.Role(g.ID, m.ID) != "" {
		t.Fatal("roles")
	}
	if err := s.SetMember(g.ID, m.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(g.ID, owner, ""); err == nil {
		t.Fatal("removing the only owner accepted")
	}
	if err := s.SetMember(g.ID, m.ID, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(g.ID, owner, ""); err != nil {
		t.Fatalf("owner can leave once another owner exists: %v", err)
	}
	if _, ok := s.GroupsOf(m.ID)[g.ID]; !ok {
		t.Fatal("GroupsOf")
	}
	// deleting a user drops their memberships
	s.CreateUser("x", "", true)
	if err := s.DeleteUser(m.ID); err != nil {
		t.Fatal(err)
	}
	g2, _ := s.Group(g.ID)
	if len(g2.Members) != 0 {
		t.Fatalf("members after delete: %v", g2.Members)
	}
}
