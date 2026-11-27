package server

import (
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/term"
)

// Guided Claude sign-in. `claude setup-token` is an interactive program: it
// prints a sign-in link, waits for the code the browser shows, then prints a
// long-lived token. Instead of making people open a shell for that, the
// server runs it in the user's Home container on a very wide pty (so
// nothing is hard-wrapped), shows the link, forwards the pasted code and
// saves the token it prints.

const (
	loginCols = 1000
	loginTTL  = 10 * time.Minute
	tokenHead = "sk-ant-oat01-"
)

var (
	ansiCSI   = regexp.MustCompile(`\x1b\[[0-9;?<>=!]*[ -/]*[@-~]`)
	ansiOSC   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	ansiOther = regexp.MustCompile(`\x1b[()#][0-9A-Za-z]|\x1b[=>78cM]`)
	osc8URL   = regexp.MustCompile(`\x1b\]8;[^;\x07\x1b]*;(https?://[^\x07\x1b\s]+)(?:\x07|\x1b\\)`)
	plainURL  = regexp.MustCompile(`https?://[^\s\x07\x1b]+`)
	tokenRe   = regexp.MustCompile(regexp.QuoteMeta(tokenHead) + `[A-Za-z0-9_-]{40,}[\r\n]`)
)

func stripANSI(s string) string {
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	return ansiOther.ReplaceAllString(s, "")
}

// officialLink reports whether a sign-in link goes to Anthropic, and so
// does the page it sends the code back to. The program runs in the user's
// Home container, where their Home agent could print a link of its own
// first, for a look-alike site.
func officialLink(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || !anthropicHost(u.Hostname()) {
		return false
	}
	for _, k := range []string{"redirect_uri", "redirect_url"} {
		if v := u.Query().Get(k); v != "" {
			r, err := neturl.Parse(v)
			if err != nil || r.Scheme != "https" || !anthropicHost(r.Hostname()) {
				return false
			}
		}
	}
	return true
}

func anthropicHost(h string) bool {
	h = strings.ToLower(h)
	for _, d := range []string{"claude.ai", "claude.com", "anthropic.com"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// parseSetupToken pulls the sign-in link and (once printed in full) the
// token out of raw `claude setup-token` terminal output.
func parseSetupToken(raw string) (url, token string) {
	for _, m := range osc8URL.FindAllStringSubmatch(raw, -1) {
		if officialLink(m[1]) {
			url = m[1]
		}
	}
	plain := stripANSI(raw)
	if url == "" {
		for _, u := range plainURL.FindAllString(plain, -1) {
			if officialLink(u) {
				url = u
				break
			}
		}
	}
	if ms := tokenRe.FindAllString(plain, -1); len(ms) > 0 {
		token = strings.TrimSpace(ms[len(ms)-1])
	}
	return url, token
}

type claudeLogin struct {
	id, uid, name string
	t             *term.Term

	mu    sync.Mutex
	state string // starting | link | saving | done | error
	url   string
	err   string
	acct  string // saved account id
}

func (l *claudeLogin) snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	host := ""
	if u, err := neturl.Parse(l.url); err == nil {
		host = u.Hostname()
	}
	return map[string]any{"id": l.id, "state": l.state, "url": l.url, "host": host, "error": l.err, "accountId": l.acct}
}

func (l *claudeLogin) set(state, errMsg string) {
	l.mu.Lock()
	l.state, l.err = state, errMsg
	l.mu.Unlock()
}

type claudeLogins struct {
	mu sync.Mutex
	m  map[string]*claudeLogin
}

func (c *claudeLogins) put(l *claudeLogin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*claudeLogin{}
	}
	c.m[l.id] = l
}

func (c *claudeLogins) get(id, uid string) (*claudeLogin, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.m[id]
	if !ok || l.uid != uid {
		return nil, false
	}
	return l, true
}

func (c *claudeLogins) drop(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// watch follows the terminal's output until the token appears, the process
// ends or the login times out.
func (s *Server) watchLogin(l *claudeLogin, save func(token string) (string, error)) {
	replay, ch, detach := l.t.Attach()
	defer detach()
	defer func() {
		l.t.Close()
		time.AfterFunc(2*time.Minute, func() { s.logins.drop(l.id) })
	}()
	raw := string(replay)
	timeout := time.NewTimer(loginTTL)
	defer timeout.Stop()
	for {
		url, token := parseSetupToken(raw)
		l.mu.Lock()
		if url != "" && l.url == "" && l.state == "starting" {
			l.url, l.state = url, "link"
		}
		l.mu.Unlock()
		if token != "" {
			l.set("saving", "")
			id, err := save(token)
			if err != nil {
				l.set("error", err.Error())
				return
			}
			l.mu.Lock()
			l.acct, l.state = id, "done"
			l.mu.Unlock()
			return
		}
		select {
		case b, ok := <-ch:
			if !ok {
				// The process ended without a token: wrong code, cancelled…
				l.mu.Lock()
				if l.state != "done" {
					l.state, l.err = "error", "Sign-in ended without a token. Start again and paste the code exactly as shown."
				}
				l.mu.Unlock()
				return
			}
			raw += string(b)
			if len(raw) > 256<<10 {
				raw = raw[len(raw)-128<<10:]
			}
		case <-timeout.C:
			l.set("error", "Sign-in timed out. Start again.")
			return
		}
	}
}

func newLoginID() string { return model.NewID() }
