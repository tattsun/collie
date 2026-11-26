// Command collie is a small self-hosted web UI for running and steering many
// Claude Code / Codex sessions from any device on your tailnet.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/server"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/store"
	"github.com/tattsun/collie/internal/term"
	"github.com/tattsun/collie/web"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", env("COLLIE_ADDR", ":7878"), "listen address (e.g. 127.0.0.1:7878 behind `tailscale serve`)")
	dataDir := flag.String("data", env("COLLIE_DATA", filepath.Join(home, ".collie")), "data directory")
	root := flag.String("root", env("COLLIE_ROOT", filepath.Join(home, "projects")), "directory where new projects are created")
	token := flag.String("token", os.Getenv("COLLIE_TOKEN"), "access token (default: generated once and stored in <data>/token)")
	noAuth := flag.Bool("no-auth", os.Getenv("COLLIE_NO_AUTH") == "1", "disable authentication (trusted networks only)")
	claudeBin := flag.String("claude", env("COLLIE_CLAUDE", "claude"), "Claude Code executable")
	codexBin := flag.String("codex", env("COLLIE_CODEX", "codex"), "Codex executable")
	idle := flag.Duration("idle-timeout", 30*time.Minute, "stop idle agent processes after this long (they resume on the next message; 0 = never)")
	showToken := flag.Bool("print-token", false, "print the access token and exit")
	loginURL := flag.Bool("login-url", false, "print sign-in links (containing the access token) and exit")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("open data dir: %v", err)
	}
	if *token == "" {
		*token, err = loadToken(filepath.Join(*dataDir, "token"))
		if err != nil {
			log.Fatalf("token: %v", err)
		}
	}
	if *showToken {
		fmt.Println(*token)
		return
	}
	if *loginURL {
		ta, err := net.ResolveTCPAddr("tcp", *addr)
		if err != nil {
			log.Fatalf("addr: %v", err)
		}
		for _, u := range signInURLs(ta, *token, *noAuth, true) {
			fmt.Println(u)
		}
		return
	}
	absRoot, _ := filepath.Abs(*root)

	hub := session.NewHub()
	mgr := session.NewManager(session.Config{
		ClaudeBin: *claudeBin, CodexBin: *codexBin, ProjectRoot: absRoot, IdleTimeout: *idle,
	}, st, hub)
	terms := term.NewManager()
	srv := server.New(mgr, terms, server.Options{Token: *token, NoAuth: *noAuth, Web: web.FS()})
	if web.FS() == nil {
		log.Printf("warning: web UI is not built (run `make`); only the API is available")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	// The token is only shown on an interactive terminal, never written to a
	// persistent log (e.g. the systemd journal).
	interactive := isTerminal(os.Stderr)
	log.Printf("collie %s listening on %s", server.Version, ln.Addr())
	for _, u := range signInURLs(ln.Addr().(*net.TCPAddr), *token, *noAuth, interactive) {
		log.Printf("  open: %s", u)
	}
	if !interactive && !*noAuth {
		log.Printf("  sign in with the token from `collie --print-token` (or get links with `collie --login-url`)")
	}
	log.Printf("projects root: %s · data: %s", absRoot, *dataDir)

	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = hs.Shutdown(ctx)
	cancel()
	terms.CloseAll()
	mgr.Shutdown()
}

func loadToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b[:])
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// signInURLs lists the URLs the server can be reached on, optionally with the
// access token (opening such a link signs the device in).
func signInURLs(a *net.TCPAddr, token string, noAuth, withToken bool) []string {
	q := "/"
	if withToken && !noAuth {
		q = "/?token=" + token
	}
	var hosts []string
	if a.IP == nil || a.IP.IsUnspecified() {
		ifaces, _ := net.InterfaceAddrs()
		for _, ia := range ifaces {
			ipn, ok := ia.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
				continue
			}
			hosts = append(hosts, ipn.IP.String())
		}
		hosts = append(hosts, "127.0.0.1")
	} else {
		hosts = append(hosts, a.IP.String())
	}
	var out []string
	for _, h := range hosts {
		tag := ""
		if ip := net.ParseIP(h); ip != nil && isTailscale(ip) {
			tag = "  ← tailscale"
		}
		out = append(out, "http://"+net.JoinHostPort(h, fmt.Sprint(a.Port))+q+tag)
	}
	return out
}

// isTailscale reports whether ip is in the CGNAT range Tailscale uses.
func isTailscale(ip net.IP) bool {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}
