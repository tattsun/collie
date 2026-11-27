// Command collie is a small self-hosted web UI for running and steering many
// Claude Code / Codex sessions from any device on your tailnet.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/auth"
	"github.com/tattsun/collie/internal/sandbox"
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
	// Run by the Home agent (inside its container): connects the agent's MCP
	// stdio to Collie's tools socket.
	if (len(os.Args) == 3 || len(os.Args) == 4) && os.Args[1] == "mcp-bridge" {
		os.Exit(mcpBridge(os.Args[2], os.Args[3:]...))
	}
	// Subcommands for the server owner (run on the server, as Collie's user).
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(subcommand(os.Args[1], os.Args[2:]))
	}
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", env("COLLIE_ADDR", ":7878"), "listen address (e.g. 127.0.0.1:7878 behind `tailscale serve`)")
	dataDir := flag.String("data", env("COLLIE_DATA", filepath.Join(home, ".collie")), "data directory")
	root := flag.String("root", env("COLLIE_ROOT", filepath.Join(home, "projects")), "directory where new projects are created")
	token := flag.String("token", os.Getenv("COLLIE_TOKEN"), "access token (default: generated once and stored in <data>/token)")
	noAuth := flag.Bool("no-auth", os.Getenv("COLLIE_NO_AUTH") == "1", "disable authentication (trusted networks only)")
	claudeBin := flag.String("claude", env("COLLIE_CLAUDE", "claude"), "Claude Code executable")
	codexBin := flag.String("codex", env("COLLIE_CODEX", "codex"), "Codex executable")
	podmanBin := flag.String("podman", env("COLLIE_PODMAN", "podman"), "Podman executable (for isolated projects)")
	ctrMemory := flag.String("container-memory", env("COLLIE_CONTAINER_MEMORY", "4g"), "default memory limit per project container")
	ctrCPUs := flag.String("container-cpus", env("COLLIE_CONTAINER_CPUS", defaultCPUs()), "default CPU limit per project container")
	ctrPids := flag.Int("container-pids", 4096, "default process limit per project container")
	proxyUID := flag.Int("trusted-proxy-uid", 0, "uid of the local proxy whose Tailscale identity headers are trusted (tailscaled: 0)")
	idle := flag.Duration("idle-timeout", 30*time.Minute, "stop idle agent processes after this long (they resume on the next message; 0 = never)")
	showToken := flag.Bool("print-token", false, "print the access token and exit")
	loginURL := flag.Bool("login-url", false, "print sign-in links (containing the access token) and exit")
	buildImage := flag.Bool("build-image", false, "build (or update) the agent container image and exit")
	publicURL := flag.String("public-url", os.Getenv("COLLIE_PUBLIC_URL"), "URL users open (passkeys are bound to it; default: the host of the first registration)")
	flag.Usage = usage
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

	sb := sandbox.New(sandbox.Config{
		Podman: *podmanBin, DataDir: *dataDir, Memory: *ctrMemory, CPUs: *ctrCPUs, Pids: *ctrPids,
	})
	if !sb.Available() {
		log.Printf("note: podman not found — new projects will run agents directly on this host")
	} else if !sb.LimitsSupported() {
		log.Printf("warning: no systemd user session — memory/CPU/process limits for isolated projects are NOT enforced (run `loginctl enable-linger %s` and start collie as a user service)", os.Getenv("USER"))
	}
	if *buildImage {
		if !sb.Available() {
			log.Fatal("podman is not installed")
		}
		log.Printf("building %s …", sb.Status().Image)
		if err := sb.BuildSync(os.Stderr); err != nil {
			log.Fatal(err)
		}
		return
	}
	users, err := auth.Open(*dataDir)
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	owner, err := users.EnsureBootstrapAdmin(os.Getenv("USER"))
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	if ou, ok := users.User(owner); ok {
		_ = sb.SeedPersonal(ou.ID, ou.Name, ou.DisplayName)
	}
	hub := session.NewHub()
	mgr := session.NewManager(session.Config{
		ClaudeBin: *claudeBin, CodexBin: *codexBin, ProjectRoot: absRoot, IdleTimeout: *idle, HostOwner: owner,
	}, st, hub, sb)
	mgr.SetAccess(users)
	terms := term.NewManager()
	srv := server.New(mgr, terms, users, server.Options{Token: *token, NoAuth: *noAuth, Web: web.FS(),
		ProxyUID: *proxyUID, PublicURL: *publicURL, Owner: owner})
	if web.FS() == nil {
		log.Printf("warning: web UI is not built (run `mise run build`); only the API is available")
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
	if users.PasskeysExist() {
		for _, u := range signInURLs(ln.Addr().(*net.TCPAddr), "", true, false) {
			log.Printf("  open: %s", u)
		}
		log.Printf("  sign in with a passkey (lost them? run `collie link` on the server)")
	} else {
		for _, u := range signInURLs(ln.Addr().(*net.TCPAddr), *token, *noAuth, interactive) {
			log.Printf("  open: %s", u)
		}
		if !interactive && !*noAuth {
			log.Printf("  sign in with the token from `collie --print-token` (or get links with `collie --login-url`), then add a passkey")
		}
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
	users.Flush()
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

// defaultCPUs gives each project container half the machine (at least 1).
func defaultCPUs() string {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	return fmt.Sprint(n)
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

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  collie [flags]                 run the server
  collie link [--user NAME]      print a one-time link to add a passkey (recovery)
  collie users                   list users
  collie make-admin NAME         make a user an admin

Flags:
`)
	flag.PrintDefaults()
}

// subcommand implements the owner's recovery tools. They edit
// <data>/auth.json directly (under its file lock); a running server picks
// the change up immediately.
func subcommand(name string, args []string) int {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dataDir := fs.String("data", env("COLLIE_DATA", filepath.Join(home, ".collie")), "data directory")
	user := fs.String("user", "", "user name (default: the first admin)")
	_ = fs.Parse(args)
	users, err := auth.Open(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch name {
	case "users":
		for _, u := range users.Users() {
			role := "member"
			if u.Admin {
				role = "admin"
			}
			if u.Disabled {
				role += ", disabled"
			}
			fmt.Printf("%-20s %-24s %-16s %d passkey(s)\n", u.Name, u.DisplayName, role, len(u.Passkeys))
		}
		return 0
	case "link":
		var target auth.User
		if *user != "" {
			u, ok := users.UserByName(*user)
			if !ok {
				fmt.Fprintf(os.Stderr, "no user %q\n", *user)
				return 1
			}
			target = u
		} else {
			for _, u := range users.Users() {
				if u.Admin && !u.Disabled {
					target = u
					break
				}
			}
		}
		if target.ID == "" {
			fmt.Fprintln(os.Stderr, "no users yet: start the server first")
			return 1
		}
		secret, err := users.CreateInvite(auth.Invite{UserID: target.ID, Note: "cli recovery"}, auth.LinkTTL)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_, origin := users.RP()
		if origin == "" {
			origin = "https://<your collie address>"
		}
		fmt.Printf("Open within %d minutes to add a passkey for %s:\n\n  %s/?invite=%s\n\n", int(auth.LinkTTL.Minutes()), target.Name, origin, secret)
		return 0
	case "make-admin":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: collie make-admin NAME")
			return 2
		}
		u, ok := users.UserByName(fs.Arg(0))
		if !ok {
			fmt.Fprintf(os.Stderr, "no user %q\n", fs.Arg(0))
			return 1
		}
		yes := true
		if _, err := users.UpdateUser(u.ID, nil, &yes, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s is now an admin\n", u.Name)
		return 0
	}
	usage()
	return 2
}

// mcpBridge copies stdin to the Unix socket and the socket to stdout until
// Collie's side is done (it finishes after the agent closes stdin). Given
// a session, it first tells Collie which session's agent it serves.
func mcpBridge(sock string, session ...string) int {
	c, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collie mcp-bridge:", err)
		return 1
	}
	defer c.Close()
	if len(session) > 0 && session[0] != "" {
		hello, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "collie/session", "params": map[string]string{"id": session[0]}})
		if _, err := c.Write(append(hello, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, "collie mcp-bridge:", err)
			return 1
		}
	}
	go func() {
		_, _ = io.Copy(c, os.Stdin)
		if uc, ok := c.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, _ = io.Copy(os.Stdout, c)
	return 0
}
