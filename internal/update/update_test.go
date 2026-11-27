package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.2.0", 1},
		{"v0.3.0", "0.3.0", 0},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.0", 1},
		{"0.3.0", "0.3.0-rc.1", 1},
		{"0.3.0-rc.2", "0.3.0-rc.1", 1},
		{"0.3.0-rc.10", "0.3.0-rc.9", 1},
		{"0.3.0-beta", "0.3.0-alpha", 1},
		{"0.3.0-4-gabc1234", "0.3.0", 1}, // built after the 0.3.0 tag
		{"0.3.1", "0.3.0-4-gabc1234", 1}, // the next release is newer still
		{"0.3.0-5-gdef5678", "0.3.0-4-gabc1234", 1},
		{"0.3.0-dirty", "0.3.0", 1},
		{"0.3.0", "dev", 1}, // anything beats an unknown build
		{"dev-abc1234", "dev", 0},
	} {
		if got := compare(parseVersion(c.a), parseVersion(c.b)); got != c.want {
			t.Errorf("compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compare(parseVersion(c.b), parseVersion(c.a)); got != -c.want {
			t.Errorf("compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	for v, rel := range map[string]bool{"0.3.0": true, "v1.2.3": true, "0.4.0-rc.1": true,
		"0.3.0-4-gabc1234": false, "0.3.0-dirty": false, "dev": false, "dev-abc1234": false} {
		if got := parseVersion(v).release(); got != rel {
			t.Errorf("release(%q) = %v, want %v", v, got, rel)
		}
	}
}

func TestSignatures(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	sums := []byte(strings.Repeat("ab", 32) + "  collie-linux-amd64\n" + strings.Repeat("cd", 32) + " *collie-linux-arm64\n")
	sig := Sign(priv, "v0.3.0", sums)

	table, err := verifySums([]ed25519.PublicKey{other, pub}, "v0.3.0", sums, sig)
	if err != nil {
		t.Fatal(err)
	}
	if table["collie-linux-amd64"] != strings.Repeat("ab", 32) || table["collie-linux-arm64"] != strings.Repeat("cd", 32) {
		t.Errorf("checksums: %v", table)
	}
	if _, err := verifySums([]ed25519.PublicKey{pub}, "v0.4.0", sums, sig); err == nil {
		t.Error("signature accepted for another tag")
	}
	tampered := append([]byte(nil), sums...)
	tampered[0] = 'f'
	if _, err := verifySums([]ed25519.PublicKey{pub}, "v0.3.0", tampered, sig); err == nil {
		t.Error("signature accepted for changed checksums")
	}
	if _, err := verifySums([]ed25519.PublicKey{other}, "v0.3.0", sums, sig); err == nil {
		t.Error("signature accepted with the wrong key")
	}
	if _, err := verifySums(nil, "v0.3.0", sums, sig); err != errNoKeys {
		t.Errorf("no keys: %v", err)
	}

	// Keys round-trip through release.pub's format.
	keys, err := ParseKeys("# comment\n\n" + b64(pub) + "\n")
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Errorf("ParseKeys: %v %v", keys, err)
	}
	if _, err := ParseKeys("not-a-key\n"); err == nil {
		t.Error("ParseKeys accepted garbage")
	}
	if _, err := ParsePrivateKey(b64(priv.Seed())); err != nil {
		t.Error(err)
	}
	// The shipped key file parses (it may hold no key yet).
	if _, err := ParseKeys(releasePub); err != nil {
		t.Errorf("release.pub: %v", err)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	for path, want := range map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/app.slice/collie.service":    "collie.service",
		"/user.slice/user-1000.slice/user@1000.service/app.slice/my-collie.service": "my-collie.service",
		"/user.slice/user-1000.slice/session-3.scope":                               "",
		"/system.slice/collie.service":                                              "",
		"/user.slice/user-1000.slice/user@1000.service":                             "",
		"/": "",
	} {
		if got := unitFromCgroup(path); got != want {
			t.Errorf("unitFromCgroup(%q) = %q, want %q", path, got, want)
		}
	}
}

// ---- the server side: check, download, verify, hand over ----

type fakeRelease struct {
	srv      *httptest.Server
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	tag      string
	files    map[string][]byte
	auth     atomic.Value // last Authorization header seen on the API
	listFail bool
}

// newFakeRelease serves a signed release whose "binary" is a script
// printing its version.
func newFakeRelease(t *testing.T, tag string) *fakeRelease {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fakeRelease{pub: pub, priv: priv, tag: tag, files: map[string][]byte{}}
	f.files[AssetName()] = []byte("#!/bin/sh\necho " + strings.TrimPrefix(tag, "v") + "\n")
	f.resign()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/releases/latest" {
			f.auth.Store(r.Header.Get("Authorization"))
			var assets []map[string]any
			for name, b := range f.files {
				assets = append(assets, map[string]any{"name": name, "url": f.srv.URL + "/assets/" + name, "size": len(b)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "body": "notes", "html_url": "https://example/rel",
				"published_at": time.Now().Format(time.RFC3339), "assets": assets})
			return
		}
		if name, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok && r.Header.Get("Accept") == "application/octet-stream" {
			if b, ok := f.files[name]; ok {
				_, _ = w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) resign() {
	bin := f.files[AssetName()]
	sum := sha256.Sum256(bin)
	f.files[assetSums] = []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), AssetName()))
	f.files[assetSig] = Sign(f.priv, f.tag, f.files[assetSums])
}

func newTestUpdater(t *testing.T, f *fakeRelease, launch func([]string) error) *Updater {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "collie")
	_ = os.MkdirAll(filepath.Dir(exe), 0o755)
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	return New(Config{Repo: "o/r", API: f.srv.URL, DataDir: filepath.Join(dir, "data"), Current: "0.2.0",
		Keys: []ed25519.PublicKey{f.pub}, Exe: exe, Unit: "collie.service", Launch: launch,
		Quiet: 50 * time.Millisecond, Poll: 10 * time.Millisecond})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestUpdaterAppliesVerifiedRelease(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	launched := make(chan []string, 1)
	u := newTestUpdater(t, f, func(args []string) error { launched <- args; return nil })
	if st := u.Status(); !st.CanApply || st.Available {
		t.Fatalf("before checking: %+v", st)
	}
	if err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if !st.Available || st.Latest != "0.3.0" || st.Notes != "notes" {
		t.Fatalf("after checking: %+v", st)
	}
	if err := u.Apply(false); err != nil {
		t.Fatal(err)
	}
	var args []string
	select {
	case args = <-launched:
	case <-time.After(5 * time.Second):
		t.Fatalf("not launched: %+v", u.Status())
	}
	if args[0] != u.cfg.Exe || args[1] != "update" || args[2] != "apply" {
		t.Errorf("launch: %v", args)
	}
	staged := argValue(args, "--staged")
	sum, err := fileSHA256(staged)
	if err != nil || sum != argValue(args, "--sha256") || argValue(args, "--to") != "0.3.0" || argValue(args, "--from") != "0.2.0" {
		t.Errorf("staged %s: %s %v; args %v", staged, sum, err, args)
	}
	if st := u.Status(); st.Phase != PhaseRestarting {
		t.Errorf("phase %q", st.Phase)
	}
}

func TestUpdaterRejectsBadReleases(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(f *fakeRelease)
		want  string // in the error
	}{
		{"tampered binary", func(f *fakeRelease) { f.files[AssetName()] = []byte("#!/bin/sh\necho evil\n") }, "does not match the signed checksum"},
		{"wrong signer", func(f *fakeRelease) { _, f.priv, _ = ed25519.GenerateKey(rand.Reader); f.resign() }, "signing key"},
		{"unsigned", func(f *fakeRelease) { delete(f.files, assetSig) }, "is not signed"},
		{"no build for us", func(f *fakeRelease) { delete(f.files, AssetName()) }, "no build for this machine"},
		{"version mismatch", func(f *fakeRelease) { f.files[AssetName()] = []byte("#!/bin/sh\necho 0.2.9\n"); f.resign() }, `is version "0.2.9"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRelease(t, "v0.3.0")
			c.spoil(f)
			u := newTestUpdater(t, f, func([]string) error { t.Error("launched a bad release"); return nil })
			if err := u.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := u.Apply(false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the update to fail", func() bool { st := u.Status(); return st.Phase == "" && st.Error != "" })
			if st := u.Status(); !strings.Contains(st.Error, c.want) {
				t.Errorf("error %q, want it to mention %q", st.Error, c.want)
			}
		})
	}
}

func TestUpdaterSendsToken(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	u := newTestUpdater(t, f, nil)
	_ = u.Check(t.Context())
	if got := f.auth.Load(); got != "" {
		t.Errorf("sent %q without a token", got)
	}
	if err := u.SetToken(" secret \n"); err != nil {
		t.Fatal(err)
	}
	_ = u.Check(t.Context())
	if got := f.auth.Load(); got != "Bearer secret" {
		t.Errorf("Authorization %q", got)
	}
	// Persisted, privately.
	fi, err := os.Stat(filepath.Join(u.dir, "settings.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings.json: %v %v", fi, err)
	}
	if u2 := New(u.cfg); !u2.Status().TokenSet {
		t.Error("token not persisted")
	}
}

func TestAutoUpdateWaitsUntilQuiet(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	launched := make(chan []string, 1)
	u := newTestUpdater(t, f, func(args []string) error { launched <- args; return nil })
	var busy atomic.Int32
	busy.Store(1)
	u.cfg.Busy = func() int { return int(busy.Load()) }
	if err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := u.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "waiting", func() bool { return u.Status().Phase == PhaseWaiting })
	select {
	case <-launched:
		t.Fatal("restarted while busy")
	case <-time.After(200 * time.Millisecond):
	}
	busy.Store(0)
	select {
	case <-launched:
	case <-time.After(5 * time.Second):
		t.Fatalf("not launched once quiet: %+v", u.Status())
	}
}

func TestAutoUpdateCanBeCalledOff(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	u := newTestUpdater(t, f, func([]string) error { t.Error("launched"); return nil })
	u.cfg.Busy = func() int { return 1 }
	_ = u.Check(t.Context())
	_ = u.SetAuto(true)
	waitFor(t, "waiting", func() bool { return u.Status().Phase == PhaseWaiting })
	_ = u.SetAuto(false)
	waitFor(t, "called off", func() bool { st := u.Status(); return st.Phase == "" && st.Error == "" })
}

func TestUpdateNowSkipsTheWait(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	launched := make(chan []string, 1)
	u := newTestUpdater(t, f, func(args []string) error { launched <- args; return nil })
	u.cfg.Busy = func() int { return 1 }
	_ = u.Check(t.Context())
	_ = u.SetAuto(true)
	waitFor(t, "waiting", func() bool { return u.Status().Phase == PhaseWaiting })
	if err := u.Apply(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("Update now did not restart")
	}
}

func TestAutoUpdateSkipsDevBuildsAndFailedVersions(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	u := newTestUpdater(t, f, func([]string) error { t.Error("launched"); return nil })
	u.cfg.Current = "0.2.0-3-gabc1234"
	_ = u.Check(t.Context())
	_ = u.SetAuto(true)
	time.Sleep(100 * time.Millisecond)
	if st := u.Status(); st.Phase != "" || !st.DevBuild {
		t.Errorf("dev build: %+v", st)
	}

	u2 := newTestUpdater(t, f, func([]string) error { t.Error("launched"); return nil })
	u2.last = &Result{From: "0.2.0", To: "0.3.0", Error: "did not start"}
	_ = u2.Check(t.Context())
	_ = u2.SetAuto(true)
	time.Sleep(100 * time.Millisecond)
	if st := u2.Status(); st.Phase != "" {
		t.Errorf("retried a version that was rolled back: %+v", st)
	}
}

func TestLaunchFailureIsReported(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	var u *Updater
	u = newTestUpdater(t, f, func(args []string) error {
		// The apply step gives up before stopping Collie and says why.
		b, _ := json.Marshal(Result{From: "0.2.0", To: "0.3.0", Error: "disk full", At: time.Now().UnixMilli()})
		return os.WriteFile(filepath.Join(u.dir, "result.json"), b, 0o600)
	})
	_ = u.Check(t.Context())
	_ = u.Apply(false)
	waitFor(t, "the failure", func() bool { st := u.Status(); return st.Phase == "" && st.Error == "disk full" })
}

func TestCannotApplyOutsideSystemd(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	u := New(Config{Repo: "o/r", API: f.srv.URL, DataDir: t.TempDir(), Current: "0.2.0", Keys: []ed25519.PublicKey{f.pub},
		Exe: "/nonexistent/collie", Unit: "", Launch: func([]string) error { return nil }})
	if st := u.Status(); st.CanApply || st.Why == "" {
		t.Errorf("%+v", st)
	}
	_ = u.Check(t.Context())
	if err := u.Apply(false); err == nil {
		t.Error("applied outside systemd")
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestResultWrittenAfterStartIsPickedUp(t *testing.T) {
	f := newFakeRelease(t, "v0.3.0")
	u := newTestUpdater(t, f, nil)
	changed := make(chan struct{}, 4)
	u.OnChange = func() { changed <- struct{}{} }
	go u.watchResult(5*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if u.Status().Last != nil {
		t.Fatal("result before any was written")
	}
	// The apply step finishes after this version started.
	_ = os.MkdirAll(u.dir, 0o700)
	b, _ := json.Marshal(Result{From: "0.1.0", To: "0.2.0", OK: true, At: time.Now().UnixMilli()})
	_ = os.WriteFile(filepath.Join(u.dir, "result.json"), b, 0o600)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("result not picked up")
	}
	if l := u.Status().Last; l == nil || !l.OK || l.To != "0.2.0" {
		t.Errorf("last = %+v", l)
	}
}
