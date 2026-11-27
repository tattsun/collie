package server

import (
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/term"
)

// Output shaped like what `claude setup-token` writes to a wide pty: cursor
// moves instead of spaces, the link as an OSC 8 hyperlink, then the token
// alone on a row.
func TestParseSetupToken(t *testing.T) {
	link := "https://claude.ai/oauth/authorize?code=true&client_id=abc&state=xyz"
	prompt := "\x1b[2GBrowser\x1b[10Gdidn't\x1b[17Gopen?\r\n\r\n" +
		"\x1b]8;id=wk5gn3;" + link + "\x07" + link + "\x1b]8;;\x07\r\n\r\n\x1b[2GPaste\x1b[8Gcode\r\n"
	url, token := parseSetupToken(prompt)
	if url != link || token != "" {
		t.Fatalf("prompt: url=%q token=%q", url, token)
	}

	tok := "sk-ant-oat01-" + strings.Repeat("aB3_-", 20)
	// Not terminated yet: the row may still be arriving.
	if _, got := parseSetupToken(prompt + "\x1b[2G✓ Long-lived token\r\n\r\n" + tok[:60]); got != "" {
		t.Fatalf("partial token accepted: %q", got)
	}
	_, got := parseSetupToken(prompt + "\x1b[2GYour\x1b[7Gtoken:\r\n\r\n" + tok + "\r\n\r\nStore it safely\r\n")
	if got != tok {
		t.Fatalf("token = %q, want %q", got, tok)
	}
}

func TestParseSetupTokenPlainURLFallback(t *testing.T) {
	url, _ := parseSetupToken("\x1b[1mVisit\x1b[0m https://claude.com/cai/oauth/authorize?x=1\r\n")
	if url != "https://claude.com/cai/oauth/authorize?x=1" {
		t.Fatalf("url = %q", url)
	}
}

// A stand-in for `claude setup-token`: prints the link, waits for a line
// (the code), then prints the token alone on a row.
func TestWatchLoginSavesToken(t *testing.T) {
	tok := "sk-ant-oat01-" + strings.Repeat("xY9_-", 20)
	script := `printf '\033]8;;https://claude.ai/oauth/authorize?x=1\007https://claude.ai/oauth/authorize?x=1\033]8;;\007\r\n'
read code; [ "$code" = "good" ] || exit 1
printf '\r\nYour token:\r\n\r\n%s\r\n\r\n' '` + tok + `'
sleep 5`
	tm := term.NewManager()
	tt, err := tm.Create(term.Spec{Cwd: t.TempDir(), Argv: []string{"sh", "-c", script}, Cols: loginCols, Rows: 30, User: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	l := &claudeLogin{id: "l1", uid: "u1", t: tt, state: "starting"}
	s.logins.put(l)
	saved := make(chan string, 1)
	go s.watchLogin(l, func(token string) (string, error) { saved <- token; return "acct1", nil })

	wait := func(want string) map[string]any {
		t.Helper()
		for i := 0; i < 100; i++ {
			if snap := l.snapshot(); snap["state"] == want {
				return snap
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("never reached %q, at %v", want, l.snapshot())
		return nil
	}
	if snap := wait("link"); snap["url"] != "https://claude.ai/oauth/authorize?x=1" {
		t.Fatalf("url = %v", snap["url"])
	}
	_ = tt.Write([]byte("good\r"))
	select {
	case got := <-saved:
		if got != tok {
			t.Fatalf("saved %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("token was not saved")
	}
	if snap := wait("done"); snap["accountId"] != "acct1" {
		t.Fatalf("snapshot = %v", snap)
	}
}

func TestWatchLoginReportsFailure(t *testing.T) {
	tm := term.NewManager()
	tt, err := tm.Create(term.Spec{Cwd: t.TempDir(), Argv: []string{"sh", "-c", "echo bad; exit 1"}, Cols: loginCols, Rows: 30, User: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	l := &claudeLogin{id: "l2", uid: "u1", t: tt, state: "starting"}
	go s.watchLogin(l, func(string) (string, error) { t.Error("must not save"); return "", nil })
	for i := 0; i < 100; i++ {
		if l.snapshot()["state"] == "error" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("state = %v", l.snapshot())
}
