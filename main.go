// Command collie is a small self-hosted web UI for running and steering many
// Claude Code / Codex sessions from any device on your tailnet.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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
	"github.com/tattsun/collie/internal/update"
	"github.com/tattsun/collie/internal/version"
	"github.com/tattsun/collie/web"
	"golang.org/x/crypto/acme/autocert"
)

// defaultUpdateRepo is where releases (and so updates) come from.
const defaultUpdateRepo = "tattsun/collie"

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
	home, _ := os.UserHomeDir()
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(update.Main(os.Args[2:], version.Version, updateRepo(env("COLLIE_UPDATE_REPO", defaultUpdateRepo)),
			env("COLLIE_DATA", filepath.Join(home, ".collie"))))
	}
	// Subcommands for the server owner (run on the server, as Collie's user).
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(subcommand(os.Args[1], os.Args[2:]))
	}
	addr := flag.String("addr", env("COLLIE_ADDR", ":7878"), "listen address (e.g. 127.0.0.1:7878 behind `tailscale serve` or a reverse proxy; default with --domain: :443)")
	domain := flag.String("domain", os.Getenv("COLLIE_DOMAIN"), "serve HTTPS for this domain name with a certificate from Let's Encrypt (needs ports 443 and 80 reachable from the internet)")
	dataDir := flag.String("data", env("COLLIE_DATA", filepath.Join(home, ".collie")), "data directory")
	root := flag.String("root", env("COLLIE_ROOT", filepath.Join(home, "projects")), "directory where new projects are created")
	token := flag.String("token", os.Getenv("COLLIE_TOKEN"), "setup token for the first sign-in, before any passkey exists (default: generated once and stored in <data>/token)")
	noAuth := flag.Bool("no-auth", os.Getenv("COLLIE_NO_AUTH") == "1", "disable sign-in, for development on this machine only (needs a loopback --addr; requests through proxies are refused)")
	claudeBin := flag.String("claude", env("COLLIE_CLAUDE", "claude"), "Claude Code executable")
	codexBin := flag.String("codex", env("COLLIE_CODEX", "codex"), "Codex executable")
	podmanBin := flag.String("podman", env("COLLIE_PODMAN", "podman"), "Podman executable (for isolated projects)")
	ctrMemory := flag.String("container-memory", env("COLLIE_CONTAINER_MEMORY", "4g"), "default memory limit per project container")
	ctrCPUs := flag.String("container-cpus", env("COLLIE_CONTAINER_CPUS", defaultCPUs()), "default CPU limit per project container")
	ctrPids := flag.Int("container-pids", 4096, "default process limit per project container")
	_ = flag.Int("trusted-proxy-uid", 0, "deprecated and ignored: Tailscale identity no longer signs anyone in")
	idle := flag.Duration("idle-timeout", 30*time.Minute, "stop idle agent processes after this long (they resume on the next message; 0 = never)")
	showToken := flag.Bool("print-token", false, "print the access token and exit")
	loginURL := flag.Bool("login-url", false, "print sign-in links (containing the access token) and exit")
	buildImage := flag.Bool("build-image", false, "build (or update) the agent container image and exit")
	prepareImage := flag.Bool("prepare-image", false, "build the agent container image if it is missing and exit (used by updates)")
	repo := flag.String("update-repo", env("COLLIE_UPDATE_REPO", defaultUpdateRepo), "GitHub repository (owner/name) whose releases Collie updates to; \"off\" to disable")
	showVersion := flag.Bool("version", false, "print the version and exit")
	publicURL := flag.String("public-url", os.Getenv("COLLIE_PUBLIC_URL"), "URL users open (passkeys are bound to it; default: the host of the first registration)")
	flag.Usage = usage
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	if *domain != "" {
		*domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(*domain), "."))
		if !addrSet() {
			*addr = ":443"
		}
		if *publicURL == "" {
			*publicURL = "https://" + *domain
		}
	}
	if *noAuth {
		// Without sign-in, whoever reaches Collie controls this server.
		if *domain != "" || !loopbackAddr(*addr) {
			log.Fatalf("--no-auth is only for development on this machine: use --addr 127.0.0.1:PORT (and no --domain)")
		}
	}

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
		for _, u := range signInURLs(ta, *publicURL, *token, *noAuth, true) {
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
	if *buildImage || *prepareImage {
		if !sb.Available() {
			log.Fatal("podman is not installed")
		}
		build := sb.BuildSync
		if *prepareImage {
			build = sb.BuildIfMissing
		} else {
			log.Printf("building %s …", sb.Status().Image)
		}
		if err := build(os.Stderr); err != nil {
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
	podman := ""
	if sb.Available() {
		podman = *podmanBin
	}
	upd := update.New(update.Config{
		Repo: updateRepo(*repo), DataDir: *dataDir, Current: version.Version, Keys: update.Keys(), Podman: podman,
		// What a restart would interrupt: agents at work (or asking something
		// in the last day) and terminals in use in the last half hour.
		Busy: func() int { return mgr.BusySessions(24*time.Hour) + terms.Active(30*time.Minute) },
	})
	srv := server.New(mgr, terms, users, server.Options{Token: *token, NoAuth: *noAuth, Web: web.FS(),
		PublicURL: *publicURL, Owner: owner, Updater: upd})
	if web.FS() == nil {
		log.Printf("warning: web UI is not built (run `mise run build`); only the API is available")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	var certs *autocert.Manager
	if *domain != "" {
		certs = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(*domain),
			Cache:      autocert.DirCache(filepath.Join(*dataDir, "acme")),
		}
		hs.TLSConfig = certs.TLSConfig()
		hs.TLSConfig.MinVersion = tls.VersionTLS12
		// HTTP/1.1 only: the terminal and live updates are WebSockets.
		var http1 http.Protocols
		http1.SetHTTP1(true)
		hs.Protocols = &http1
		// Scanners probe every public address; their failed handshakes are noise.
		hs.ErrorLog = log.New(quietHandshakes{}, "", log.LstdFlags)
	}

	// The token is only shown on an interactive terminal, never written to a
	// persistent log (e.g. the systemd journal).
	interactive := isTerminal(os.Stderr)
	log.Printf("collie %s listening on %s", version.Version, ln.Addr())
	if users.PasskeysExist() {
		for _, u := range signInURLs(ln.Addr().(*net.TCPAddr), *publicURL, "", true, false) {
			log.Printf("  open: %s", u)
		}
		log.Printf("  sign in with a passkey (lost them? run `collie link` on the server)")
	} else {
		for _, u := range signInURLs(ln.Addr().(*net.TCPAddr), *publicURL, *token, *noAuth, interactive) {
			log.Printf("  open: %s", u)
		}
		if !interactive && !*noAuth {
			log.Printf("  sign in with the token from `collie --print-token` (or get links with `collie --login-url`), then add a passkey")
		}
	}
	log.Printf("projects root: %s · data: %s", absRoot, *dataDir)

	go func() {
		var err error
		if certs != nil {
			err = hs.ServeTLS(ln, "", "")
		} else {
			err = hs.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()
	if certs != nil {
		go serveHTTPRedirect(certs)
	}
	// Up and serving: an update waiting for this version to start can finish.
	if err := update.MarkRunning(*dataDir, version.Version); err != nil {
		log.Printf("update marker: %v", err)
	}
	upd.Start()
	go sb.PruneImages()

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

// addrSet reports whether --addr / COLLIE_ADDR was given.
func addrSet() bool {
	set := os.Getenv("COLLIE_ADDR") != ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			set = true
		}
	})
	return set
}

// loopbackAddr reports whether a listen address only accepts connections
// from this machine.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// quietHandshakes drops "TLS handshake error" lines from the server log.
type quietHandshakes struct{}

func (quietHandshakes) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "TLS handshake error") {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// serveHTTPRedirect answers plain HTTP on port 80 for --domain: Let's
// Encrypt's HTTP challenge, and a redirect to HTTPS for everything else.
// The certificate also works without it (through the TLS challenge on 443).
func serveHTTPRedirect(m *autocert.Manager) {
	hs := &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	if err := hs.ListenAndServe(); err != nil {
		log.Printf("note: no HTTP→HTTPS redirect on port 80 (%v); open Collie with https://", err)
	}
}

func updateRepo(s string) string {
	if s == "off" || s == "none" {
		return ""
	}
	return s
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
// setup token (opening such a link signs the device in).
func signInURLs(a *net.TCPAddr, publicURL, token string, noAuth, withToken bool) []string {
	q := "/"
	if withToken && !noAuth {
		q = "/?token=" + token
	}
	if publicURL != "" {
		return []string{strings.TrimSuffix(publicURL, "/") + q}
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
  collie reset-passkeys --yes    forget every passkey and sign-in (to move Collie to a new address)
  collie update                  show the running and the latest release

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
	yes := fs.Bool("yes", false, "confirm (reset-passkeys)")
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
	case "reset-passkeys":
		// Passkeys only work on the address they were created on.
		if !*yes {
			fmt.Fprintln(os.Stderr, "This removes every user's passkeys and signs everyone out, so Collie can move to a new address.\n"+
				"Until you create a new passkey, the setup token (`collie --print-token`) signs in as the first admin.\n"+
				"Run again with --yes to do it.")
			return 1
		}
		if err := users.ResetRP(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("Done. Open Collie at its new address, sign in with the setup token (`collie --print-token`) and create your passkey right away.\n" +
			"Then give everyone else a link to add theirs: Admin → Users → Passkey recovery link.")
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
