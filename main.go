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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/auth"
	"github.com/tattsun/collie/internal/egress"
	"github.com/tattsun/collie/internal/preview"
	"github.com/tattsun/collie/internal/push"
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

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

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
	// The main process of a container on a network allowlist: offers
	// Collie's proxy (its socket) on the container's loopback.
	if len(os.Args) == 4 && os.Args[1] == "net-bridge" {
		err := egress.Bridge(os.Args[2], os.Args[3])
		fmt.Fprintln(os.Stderr, "collie net-bridge:", err)
		os.Exit(1)
	}
	// Run by Collie in a container (podman exec): carries Collie's
	// connections to the servers listening there (package preview).
	if len(os.Args) > 1 && os.Args[1] == "port-bridge" {
		os.Exit(portBridge(os.Args[2:]))
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
	buildImage := flag.Bool("build-image", false, "build (or update) the agent container images (Collie's and the admins' custom ones) and exit")
	prepareImage := flag.Bool("prepare-image", false, "build the agent container image if it is missing and exit (used by updates)")
	repo := flag.String("update-repo", env("COLLIE_UPDATE_REPO", defaultUpdateRepo), "GitHub repository (owner/name) whose releases Collie updates to; \"off\" to disable")
	showVersion := flag.Bool("version", false, "print the version and exit")
	publicURL := flag.String("public-url", os.Getenv("COLLIE_PUBLIC_URL"), "URL users open (passkeys are bound to it; default: the host of the first registration)")
	previewAddr := flag.String("preview-addr", os.Getenv("COLLIE_PREVIEW_ADDR"), "listen address of the first preview port for the agents' servers (default: the port after --addr's, or :8443 with --domain; \"off\" turns previews off)")
	previewURL := flag.String("preview-url", os.Getenv("COLLIE_PREVIEW_URL"), "where users open previews, on other origins than Collie's: :PORT for other ports of Collie's own host name (e.g. :8443, :8443-8446 or :8443,10000; needed behind a reverse proxy or tailscale serve), or a wildcard such as https://*.preview.example.com for a host name of its own per project container")
	previewCount := flag.Int("preview-ports", envInt("COLLIE_PREVIEW_PORTS", 4), "how many preview ports, in a row from --preview-addr's: a browser can have one preview open on each")
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
	pv, err := previewSetup(*addr, *domain, *publicURL, *previewAddr, *previewURL, *previewCount, *noAuth)
	if err != nil {
		log.Fatal(err)
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
	if !sandbox.Supported() {
		log.Printf("note: isolated projects need Linux for now — agents run directly on this machine (%s)", runtime.GOOS)
	} else if !sb.Available() {
		log.Printf("note: podman not found — new projects will run agents directly on this host")
	} else if !sb.LimitsSupported() {
		log.Printf("warning: no systemd user session — memory/CPU/process limits for isolated projects are NOT enforced (run `loginctl enable-linger %s` and start collie as a user service)", os.Getenv("USER"))
	}
	if *buildImage || *prepareImage {
		if !sandbox.Supported() {
			log.Fatalf("isolated projects need Linux for now (this is %s)", runtime.GOOS)
		}
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
	pushSvc, err := push.Open(*dataDir)
	if err != nil {
		log.Printf("notifications while Collie is closed are off: %v", err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// The preview ports: as many of them in a row as can be had.
	var plns []net.Listener
	var previewPorts []int
	if pv.addr != "" {
		host, first, _ := net.SplitHostPort(pv.addr)
		base, _ := strconv.Atoi(first)
		for i := range pv.n {
			a := net.JoinHostPort(host, strconv.Itoa(base+i))
			l, err := net.Listen("tcp", a)
			if err != nil {
				// If Tailscale or a proxy forwards a port to this one, whatever
				// listens here instead is served on Collie's name, and browsers
				// send it Collie's cookies.
				log.Printf("warning: previews of the agents' servers get %d port(s), not %d: %v. "+
					"Free that port, or lower --preview-ports and the ports your Tailscale or proxy forwards.", i, pv.n, err)
				break
			}
			plns = append(plns, l)
			previewPorts = append(previewPorts, l.Addr().(*net.TCPAddr).Port)
		}
		if len(plns) == 0 {
			pv.url = "off"
		}
		if pv.public != nil {
			pv.public = pv.public[:len(plns)]
		}
	}
	ownPorts := append([]int{ln.Addr().(*net.TCPAddr).Port}, previewPorts...)
	if *domain != "" {
		ownPorts = append(ownPorts, 80)
	}
	srv := server.New(mgr, terms, users, server.Options{Token: *token, NoAuth: *noAuth, Web: web.FS(),
		PublicURL: *publicURL, Owner: owner, Updater: upd, Push: pushSvc,
		PreviewURL: pv.url, PreviewPorts: previewPorts, PreviewPublic: pv.public, OwnPorts: ownPorts})
	if web.FS() == nil {
		log.Printf("warning: web UI is not built (run `mise run build`); only the API is available")
	}

	var handler http.Handler = srv.Handler()
	if pv.wild != "" {
		// The containers' own preview names, on Collie's HTTPS port.
		handler = byHost{suffix: pv.wild, match: srv.PreviewHandler(0), other: handler}
	}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	var phs []*http.Server
	for i := range plns {
		phs = append(phs, &http.Server{Handler: srv.PreviewHandler(i), ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10})
	}
	var certs *autocert.Manager
	if *domain != "" {
		certs = &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			HostPolicy: func(_ context.Context, host string) error {
				// Collie's name, and the preview names of containers in use.
				if strings.EqualFold(host, *domain) || srv.PreviewHost(host) {
					return nil
				}
				return fmt.Errorf("acme/autocert: host %q not configured", host)
			},
			Cache: autocert.DirCache(filepath.Join(*dataDir, "acme")),
		}
		for _, s := range append([]*http.Server{hs}, phs...) {
			s.TLSConfig = certs.TLSConfig()
			s.TLSConfig.MinVersion = tls.VersionTLS12
			// HTTP/1.1 only: the terminal, live updates and dev servers' hot
			// reload are WebSockets.
			var http1 http.Protocols
			http1.SetHTTP1(true)
			s.Protocols = &http1
			// Scanners probe every public address; their failed handshakes are noise.
			s.ErrorLog = log.New(quietHandshakes{}, "", log.LstdFlags)
		}
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
	switch {
	case pv.url == "off":
	case len(plns) > 0 && pv.url != "":
		log.Printf("previews of the agents' servers: %s, %d port(s) (served from %s)", pv.url, len(plns), plns[0].Addr())
	case len(plns) > 0:
		log.Printf("previews of the agents' servers: ports %d-%d of the address Collie is opened at", previewPorts[0], previewPorts[len(previewPorts)-1])
	case pv.wild != "":
		log.Printf("previews of the agents' servers: %s", pv.url)
	}

	serve := func(hs *http.Server, ln net.Listener) {
		var err error
		if certs != nil {
			err = hs.ServeTLS(ln, "", "")
		} else {
			err = hs.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}
	go serve(hs, ln)
	for i, h := range phs {
		go serve(h, plns[i])
	}
	if certs != nil {
		go serveHTTPRedirect(certs)
	}
	// Up and serving: an update waiting for this version to start can finish.
	if err := update.MarkRunning(*dataDir, version.Version); err != nil {
		log.Printf("update marker: %v", err)
	}
	upd.Start()
	go func() {
		sb.PruneImages()
		sb.BuildStale() // e.g. a custom image whose build failed while this version was being prepared
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = hs.Shutdown(ctx)
	for _, h := range phs {
		_ = h.Shutdown(ctx)
	}
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

// previewConf is where previews of the agents' servers are served.
type previewConf struct {
	// addr is the first preview port's listener ("": none); the others
	// follow on the next ports, n in all.
	addr string
	n    int
	url  string // for server.Options.PreviewURL
	// public are the ports browsers use, one per listener (nil: the
	// listeners' own).
	public []int
	// wild is the host name suffix of per-container preview names served
	// on Collie's main (HTTPS) listener (--domain), e.g. ".preview.example.com".
	wild string
}

// maxPreviewPorts bounds --preview-ports.
const maxPreviewPorts = 16

// previewSetup works out the preview origins from the flags. Previews get
// a few ports next to Collie's by default (with --domain: from 8443), which
// works the same on a LAN, over Tailscale and on a domain, with the same
// certificate; each port is an origin, and a browser can have one preview
// open on each. A wildcard --preview-url (https://*.preview.example.com)
// gives every container a host name, and so an origin, of its own. One
// other host name for previews is refused: the browser wouldn't send
// Collie's cookie there, which is how previews on Collie's name check that
// a link is opened by the browser it was made for (see package preview).
func previewSetup(addr, domain, publicURL, pAddr, pURL string, n int, noAuth bool) (previewConf, error) {
	if pAddr == "off" || pURL == "off" {
		return previewConf{url: "off"}, nil
	}
	if n < 1 || n > maxPreviewPorts {
		return previewConf{}, fmt.Errorf("--preview-ports %d: give 1 to %d", n, maxPreviewPorts)
	}
	c := previewConf{addr: pAddr, url: pURL, n: n}
	bad := fmt.Errorf("--preview-url %q: give :PORT (other ports of Collie's own name, e.g. :8443, :8443-8446 or :8443,10000), "+
		"a URL on Collie's own name with a port of its own, or a wildcard such as https://*.preview.example.com", pURL)
	own, ownPort := domain, ""
	if domain != "" {
		ownPort = "443"
	} else if pu, err := url.Parse(publicURL); err == nil && pu.Host != "" {
		own, ownPort = strings.ToLower(pu.Hostname()), pu.Port()
		if ownPort == "" {
			ownPort = map[string]string{"https": "443", "http": "80"}[pu.Scheme]
		}
	}
	switch {
	case strings.HasPrefix(pURL, ":"):
		ports, err := parsePorts(pURL[1:])
		if err != nil {
			return c, bad
		}
		c.url = ":"
		if c.public, err = followOn(ports, n); err != nil {
			return c, bad
		}
		c.n = len(c.public)
	case pURL != "":
		u, err := url.Parse(pURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return c, bad
		}
		c.url = u.Scheme + "://" + strings.ToLower(u.Host)
		name := strings.ToLower(u.Hostname())
		if suffix, ok := strings.CutPrefix(name, "*"); ok {
			if !strings.HasPrefix(suffix, ".") || strings.Contains(suffix[1:], "*") || strings.Count(suffix, ".") < 2 {
				return c, bad
			}
			if own != "" && (strings.HasSuffix(own, suffix) || own == suffix[1:]) {
				// Collie's own name would be one of the previews', or their parent.
				return c, fmt.Errorf("--preview-url %q: the preview names must not include Collie's own (%s); use names under another domain", pURL, own)
			}
			c.n = 1 // an origin per container already
			if domain != "" && u.Scheme == "https" && (u.Port() == "" || u.Port() == "443") {
				// On Collie's own HTTPS port, by name: no listener of their
				// own unless asked for one.
				c.wild = suffix
				if pAddr == "" {
					c.n = 0
					return c, nil
				}
			}
			break
		}
		// Collie's own name, on ports of its own.
		port, _ := strconv.Atoi(u.Port())
		if !strings.EqualFold(name, own) || port == 0 || u.Port() == ownPort {
			return c, bad
		}
		if c.public, err = followOn([]int{port}, n); err != nil {
			return c, bad
		}
	}
	if c.addr == "" {
		c.addr = nextPort(addr)
		if domain != "" {
			c.addr = ":8443"
		}
	}
	_, first, err := net.SplitHostPort(c.addr)
	base, _ := strconv.Atoi(first)
	if err != nil || base < 0 || base+c.n-1 > 65535 {
		return c, fmt.Errorf("--preview-addr %q: no room for %d ports there", c.addr, c.n)
	}
	if base == 0 && c.n > 1 {
		return c, fmt.Errorf("--preview-addr %q: give a port (the next %d ports are used too)", c.addr, c.n-1)
	}
	if domain != "" && c.url == "" {
		c.url = "https://" + net.JoinHostPort(domain, first)
	}
	if noAuth && !loopbackAddr(c.addr) {
		return c, fmt.Errorf("--no-auth: --preview-addr must be a loopback address too")
	}
	return c, nil
}

// parsePorts reads "8443", "8443-8446" or "8443,10000".
func parsePorts(v string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(v, ",") {
		a, b, isRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(strings.TrimSpace(a))
		if err != nil {
			return nil, err
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
				return nil, err
			}
		}
		if lo < 1 || hi > 65535 || hi < lo || len(out)+hi-lo+1 > maxPreviewPorts {
			return nil, errors.New("bad ports")
		}
		for p := lo; p <= hi; p++ {
			out = append(out, p)
		}
	}
	return out, nil
}

// followOn turns one port into n consecutive ones; a list is kept as it is.
func followOn(ports []int, n int) ([]int, error) {
	if len(ports) != 1 {
		return ports, nil
	}
	if ports[0]+n-1 > 65535 {
		return nil, errors.New("bad ports")
	}
	out := make([]int, n)
	for i := range out {
		out[i] = ports[0] + i
	}
	return out, nil
}

// nextPort is the listen address with the port after addr's.
func nextPort(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n >= 65535 {
		return net.JoinHostPort(host, "0")
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1))
}

// byHost sends requests for host names ending in suffix to their own
// handler.
type byHost struct {
	suffix       string
	match, other http.Handler
}

func (b byHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := r.Host
	if hn, _, err := net.SplitHostPort(h); err == nil {
		h = hn
	}
	if strings.HasSuffix(strings.ToLower(h), b.suffix) {
		b.match.ServeHTTP(w, r)
		return
	}
	b.other.ServeHTTP(w, r)
}

// portBridge is `collie port-bridge [--skip PORT]…`.
func portBridge(args []string) int {
	f := preview.Filter{UID: -1}
	for i := 0; i < len(args); i++ {
		n := 0
		if args[i] == "--skip" && i+1 < len(args) {
			n, _ = strconv.Atoi(args[i+1])
			i++
		}
		if !preview.ValidPort(n) {
			fmt.Fprintln(os.Stderr, "usage: collie port-bridge [--skip PORT]…")
			return 2
		}
		f.SkipPorts = append(f.SkipPorts, n)
	}
	if err := preview.ServeStdio(f); err != nil {
		fmt.Fprintln(os.Stderr, "collie port-bridge:", err)
		return 1
	}
	return 0
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
