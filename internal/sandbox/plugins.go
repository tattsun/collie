package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Claude Code plugins are defined at five levels, merged in this order (a
// later level overrides an earlier one for the same plugin, so false turns
// off a plugin enabled above):
//
//	global    shared/claude/plugins.json               admins
//	group     groups/<id>/plugins.json                 the group's owners; its projects
//	personal  users/<id>/profile/claude/plugins.json   the user; all of their projects
//	project   homes/<project>.plugins.json             the project's managers
//	repo      .claude/settings.json and settings.local.json in the session's directory
//
// The files use Claude Code's own keys (enabledPlugins, extraKnownMarketplaces).
// Enabling a plugin does not make Claude Code install it, so before a Claude
// agent starts Collie installs what is missing — and removes what it installed
// earlier and is no longer wanted — with `claude plugin` inside the user's
// container. Plugins therefore live in the container home, i.e. per
// (project, user). Ones installed by hand in a terminal are left alone, and
// false only means "Collie does not install it".

// BuiltinMarketplace is Anthropic's plugin directory, which Claude Code
// knows without adding it.
const BuiltinMarketplace = "anthropic-plugin-directory"

// Plugin levels, in merge order.
const (
	LevelGlobal   = "global"
	LevelGroup    = "group"
	LevelPersonal = "personal"
	LevelProject  = "project"
	LevelRepo     = "repo"
)

type PluginSet struct {
	EnabledPlugins         map[string]bool        `json:"enabledPlugins,omitempty"`
	ExtraKnownMarketplaces map[string]Marketplace `json:"extraKnownMarketplaces,omitempty"`
}

type Marketplace struct {
	Source MarketplaceSource `json:"source"`
}

// MarketplaceSource is where a marketplace comes from, as in Claude Code's
// settings: {"source": "github", "repo": "owner/repo"}, {"source": "git",
// "url": …} or {"source": "url", "url": …/marketplace.json}. A repository
// may also point at a marketplace inside itself: {"source": "directory",
// "path": …}.
type MarketplaceSource struct {
	Source string `json:"source"`
	Repo   string `json:"repo,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Level is one level's definition.
type Level struct {
	Name string
	Set  PluginSet
}

// EffectivePlugin is a plugin after merging the levels.
type EffectivePlugin struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	From    string `json:"from"` // the level that decided
}

const maxPlugins = 200

var (
	pluginIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}@[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	mktNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	ghRepoRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)
	safeIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	sourceURLRe = regexp.MustCompile(`^(?:(?:https?|ssh|git)://[^\s]+|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+)$`)
)

// Validate checks a definition edited in the UI. (Repositories' settings are
// filtered by the same rules when read, see clean.)
func (s PluginSet) Validate() error {
	if len(s.EnabledPlugins) > maxPlugins {
		return fmt.Errorf("too many plugins (max %d)", maxPlugins)
	}
	for id := range s.EnabledPlugins {
		if !pluginIDRe.MatchString(id) {
			return fmt.Errorf("invalid plugin %q (use name@marketplace)", id)
		}
	}
	for name, mk := range s.ExtraKnownMarketplaces {
		if err := validMarketplace(name, mk.Source, false); err != nil {
			return err
		}
	}
	return nil
}

func validMarketplace(name string, src MarketplaceSource, allowDir bool) error {
	if !mktNameRe.MatchString(name) || name == BuiltinMarketplace {
		return fmt.Errorf("invalid marketplace name %q", name)
	}
	if _, err := src.arg(allowDir); err != nil {
		return fmt.Errorf("marketplace %s: %v", name, err)
	}
	return nil
}

// arg is the source as `claude plugin marketplace add` takes it. Nothing
// returned starts with "-" (it is passed as an argument).
func (s MarketplaceSource) arg(allowDir bool) (string, error) {
	switch s.Source {
	case "github":
		if ghRepoRe.MatchString(s.Repo) {
			return s.Repo, nil
		}
		return "", errors.New("a github source needs repo: owner/repo")
	case "git", "url":
		if sourceURLRe.MatchString(s.URL) {
			return s.URL, nil
		}
		return "", errors.New("needs a URL (https://…)")
	case "directory":
		if allowDir && filepath.IsAbs(s.Path) {
			return filepath.Clean(s.Path), nil
		}
		if allowDir {
			return "", errors.New("needs a path")
		}
	}
	return "", fmt.Errorf("unsupported source %q (use github, git or url)", s.Source)
}

// clean drops what Validate would reject, reporting each problem.
func (s PluginSet) clean(allowDir bool) (PluginSet, []error) {
	var errs []error
	out := PluginSet{EnabledPlugins: map[string]bool{}, ExtraKnownMarketplaces: map[string]Marketplace{}}
	for _, id := range slices.Sorted(maps.Keys(s.EnabledPlugins)) {
		if !pluginIDRe.MatchString(id) {
			errs = append(errs, fmt.Errorf("invalid plugin %q (use name@marketplace)", id))
			continue
		}
		if len(out.EnabledPlugins) >= maxPlugins {
			errs = append(errs, fmt.Errorf("too many plugins (max %d)", maxPlugins))
			break
		}
		out.EnabledPlugins[id] = s.EnabledPlugins[id]
	}
	for _, name := range slices.Sorted(maps.Keys(s.ExtraKnownMarketplaces)) {
		mk := s.ExtraKnownMarketplaces[name]
		if err := validMarketplace(name, mk.Source, allowDir); err != nil {
			errs = append(errs, err)
			continue
		}
		out.ExtraKnownMarketplaces[name] = mk
	}
	return out, errs
}

// MergePlugins merges the levels in order. Marketplaces declared at a later
// level replace earlier ones of the same name.
func MergePlugins(levels []Level) ([]EffectivePlugin, map[string]MarketplaceSource) {
	byID := map[string]EffectivePlugin{}
	mkts := map[string]MarketplaceSource{}
	for _, l := range levels {
		for id, on := range l.Set.EnabledPlugins {
			byID[id] = EffectivePlugin{ID: id, Enabled: on, From: l.Name}
		}
		for name, mk := range l.Set.ExtraKnownMarketplaces {
			mkts[name] = mk.Source
		}
	}
	out := make([]EffectivePlugin, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, mkts
}

// ---- where the levels are kept ----

func (m *Manager) GlobalPluginsPath() string {
	return filepath.Join(m.SharedPath(), "claude", "plugins.json")
}

func (m *Manager) PersonalPluginsPath(userID string) string {
	return filepath.Join(m.PersonalDir(userID), "claude", "plugins.json")
}

func (m *Manager) groupDir(groupID string) string {
	return filepath.Join(m.cfg.DataDir, "groups", groupID)
}

func (m *Manager) GroupPluginsPath(groupID string) string {
	return filepath.Join(m.groupDir(groupID), "plugins.json")
}

func (m *Manager) ProjectPluginsPath(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "homes", projectID+".plugins.json")
}

// RemoveGroup deletes a group's data (its plugin definition).
func (m *Manager) RemoveGroup(groupID string) {
	if safeIDRe.MatchString(groupID) {
		_ = os.RemoveAll(m.groupDir(groupID))
	}
}

// ReadPluginSet reads a level's definition ("" content or no file: empty).
func ReadPluginSet(path string) (PluginSet, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PluginSet{}, nil
	}
	if err != nil {
		return PluginSet{}, err
	}
	var s PluginSet
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return PluginSet{}, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	return s, nil
}

// WritePluginSet validates and replaces a level's definition.
func WritePluginSet(path string, s PluginSet) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := path + ".collie-tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readRepoPlugins reads the plugin keys of the repository's Claude Code
// settings in dir (settings.local.json over settings.json). The directory
// is the agent's to write, so it is read through os.Root and only what
// passes validation is kept; relative directory marketplaces are resolved
// against dir.
func readRepoPlugins(dir string) (PluginSet, []error) {
	out := PluginSet{EnabledPlugins: map[string]bool{}, ExtraKnownMarketplaces: map[string]Marketplace{}}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out, nil
	}
	defer root.Close()
	var errs []error
	for _, name := range []string{".claude/settings.json", ".claude/settings.local.json"} {
		b, err := root.ReadFile(name)
		if err != nil || len(b) > 1<<20 {
			continue
		}
		var raw struct {
			EnabledPlugins         map[string]any             `json:"enabledPlugins"`
			ExtraKnownMarketplaces map[string]json.RawMessage `json:"extraKnownMarketplaces"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", name, err))
			continue
		}
		for id, v := range raw.EnabledPlugins {
			if on, ok := v.(bool); ok {
				out.EnabledPlugins[id] = on
			}
		}
		for mk, v := range raw.ExtraKnownMarketplaces {
			var decl Marketplace
			if json.Unmarshal(v, &decl) != nil {
				errs = append(errs, fmt.Errorf("%s: marketplace %s: unreadable source", name, mk))
				continue
			}
			if decl.Source.Source == "directory" && decl.Source.Path != "" && !filepath.IsAbs(decl.Source.Path) {
				decl.Source.Path = filepath.Join(dir, decl.Source.Path)
			}
			out.ExtraKnownMarketplaces[mk] = decl
		}
	}
	cleaned, cerrs := out.clean(true)
	return cleaned, append(errs, cerrs...)
}

// PluginLevels are the definitions that apply to the user's agents in the
// project, in merge order; cwd is the agent's directory, whose repository
// settings come last ("" leaves them out). Problems (unreadable files,
// invalid entries, which are skipped) are returned alongside.
func (m *Manager) PluginLevels(p model.Project, userID, cwd string) ([]Level, []error) {
	var levels []Level
	var errs []error
	add := func(name, path string) {
		s, err := ReadPluginSet(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s plugins: %v", name, err))
			return
		}
		s, cerrs := s.clean(false)
		for _, e := range cerrs {
			errs = append(errs, fmt.Errorf("%s plugins: %v", name, e))
		}
		levels = append(levels, Level{Name: name, Set: s})
	}
	add(LevelGlobal, m.GlobalPluginsPath())
	if p.OwnerGroup != "" {
		add(LevelGroup, m.GroupPluginsPath(p.OwnerGroup))
	}
	add(LevelPersonal, m.PersonalPluginsPath(userID))
	add(LevelProject, m.ProjectPluginsPath(p.ID))
	if cwd != "" {
		s, rerrs := readRepoPlugins(cwd)
		for _, e := range rerrs {
			errs = append(errs, fmt.Errorf("repository plugins: %v", e))
		}
		levels = append(levels, Level{Name: LevelRepo, Set: s})
	}
	return levels, errs
}

// ---- installing into a container ----

// What Collie finds in a container home (all read on the host).
type homePlugins struct {
	installed map[string]bool // installed at user scope
	markets   map[string]bool // added marketplaces
	managed   map[string]bool // installed by Collie
}

const managedFile = ".claude/collie-plugins.json"

func readHomePlugins(root *os.Root) homePlugins {
	h := homePlugins{installed: map[string]bool{}, markets: map[string]bool{}, managed: map[string]bool{}}
	read := func(name string, v any) {
		if b, err := root.ReadFile(name); err == nil && len(b) < 8<<20 {
			_ = json.Unmarshal(b, v)
		}
	}
	var inst struct {
		Plugins map[string][]struct {
			Scope string `json:"scope"`
		} `json:"plugins"`
	}
	read(".claude/plugins/installed_plugins.json", &inst)
	for id, entries := range inst.Plugins {
		for _, e := range entries {
			if e.Scope == "user" {
				h.installed[id] = true
			}
		}
	}
	var mk map[string]json.RawMessage
	read(".claude/plugins/known_marketplaces.json", &mk)
	for name := range mk {
		h.markets[name] = true
	}
	var st struct {
		Managed []string `json:"managed"`
	}
	read(managedFile, &st)
	for _, id := range st.Managed {
		h.managed[id] = true
	}
	return h
}

func writeManaged(root *os.Root, managed map[string]bool) error {
	b, _ := json.MarshalIndent(map[string]any{"managed": slices.Sorted(maps.Keys(managed))}, "", "  ")
	tmp := managedFile + ".collie-tmp"
	_ = root.Remove(tmp)
	if err := root.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return root.Rename(tmp, managedFile)
}

type pluginPlan struct {
	install   []string
	uninstall []string
	managed   map[string]bool // Collie's plugins that stay
}

// planPlugins: install what is wanted and missing; uninstall what Collie
// installed and is no longer wanted. Plugins installed by hand are never
// removed, and do not become Collie's when they are also wanted.
func planPlugins(want []string, h homePlugins) pluginPlan {
	p := pluginPlan{managed: map[string]bool{}}
	wanted := map[string]bool{}
	for _, id := range want {
		wanted[id] = true
		if !h.installed[id] {
			p.install = append(p.install, id)
		}
	}
	for id := range h.managed {
		switch {
		case !h.installed[id]:
			// removed by hand: forget it (reinstalled above if still wanted)
		case wanted[id]:
			p.managed[id] = true
		default:
			p.uninstall = append(p.uninstall, id)
		}
	}
	sort.Strings(p.install)
	sort.Strings(p.uninstall)
	return p
}

// SyncPlugins brings the plugins in the user's (running) container for the
// project in line with levels. notify, if set, gets progress and problems
// worth showing the user; the problems are also returned.
func (m *Manager) SyncPlugins(p model.Project, userID string, levels []Level, notify func(string)) error {
	if notify == nil {
		notify = func(string) {}
	}
	l := m.lock("plugins/" + p.ID + "/" + userID)
	l.Lock()
	defer l.Unlock()

	eff, mkts := MergePlugins(levels)
	var want []string
	for _, e := range eff {
		if e.Enabled {
			want = append(want, e.ID)
		}
	}
	root, err := os.OpenRoot(m.HomeDir(p.ID, userID))
	if err != nil {
		return err
	}
	defer root.Close()
	h := readHomePlugins(root)
	plan := planPlugins(want, h)
	if len(plan.install) == 0 && len(plan.uninstall) == 0 {
		if !maps.Equal(plan.managed, h.managed) {
			return writeManaged(root, plan.managed)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var errs []error
	report := func(err error) {
		errs = append(errs, err)
		notify(err.Error())
	}
	if len(plan.install) > 0 {
		notify("Installing Claude plugins: " + strings.Join(plan.install, ", "))
	}
	added, failed := map[string]bool{}, map[string]bool{}
	for _, id := range plan.install {
		mkt := id[strings.LastIndex(id, "@")+1:]
		if mkt != BuiltinMarketplace && !h.markets[mkt] && !added[mkt] {
			if failed[mkt] {
				continue
			}
			src, ok := mkts[mkt]
			if !ok {
				failed[mkt] = true
				report(fmt.Errorf("Plugin %s: unknown marketplace %s (add it to the plugin settings)", id, mkt))
				continue
			}
			arg, err := src.arg(true)
			if err == nil {
				var res cliResult
				res, err = m.pluginCLI(ctx, p.ID, userID, "marketplace", "add", arg)
				if err == nil && res.Marketplace != "" && res.Marketplace != mkt {
					err = fmt.Errorf("it is named %s, not %s", res.Marketplace, mkt)
				}
			}
			if err != nil {
				failed[mkt] = true
				report(fmt.Errorf("Marketplace %s could not be added: %v", mkt, err))
				continue
			}
			added[mkt] = true
		}
		if _, err := m.pluginCLI(ctx, p.ID, userID, "install", id, "--scope", "user"); err != nil {
			report(fmt.Errorf("Plugin %s could not be installed: %v", id, err))
			continue
		}
		plan.managed[id] = true
	}
	if len(plan.uninstall) > 0 {
		notify("Removing Claude plugins: " + strings.Join(plan.uninstall, ", "))
	}
	for _, id := range plan.uninstall {
		if _, err := m.pluginCLI(ctx, p.ID, userID, "uninstall", id, "--scope", "user"); err != nil {
			plan.managed[id] = true // still Collie's: try again next time
			report(fmt.Errorf("Plugin %s could not be removed: %v", id, err))
		}
	}
	if err := writeManaged(root, plan.managed); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cliResult is the line `claude plugin … --json` prints.
type cliResult struct {
	Outcome     string `json:"outcome"`
	Message     string `json:"message"`
	Marketplace string `json:"marketplace"`
}

// pluginCLI runs `claude plugin <args> --json` in the user's container.
func (m *Manager) pluginCLI(ctx context.Context, projectID, userID string, args ...string) (cliResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	argv := append(append([]string{"claude", "plugin"}, args...), "--json")
	argv, kill := m.Exec(projectID, userID, Home, "plugins-"+randomHex(6), false, nil, argv)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	stop := context.AfterFunc(ctx, func() { kill("KILL") })
	err := cmd.Run()
	stop()
	var res cliResult
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "{") {
			_ = json.Unmarshal([]byte(line), &res)
		}
	}
	if ctx.Err() != nil {
		return res, errors.New("timed out")
	}
	if err != nil || (res.Outcome != "" && res.Outcome != "ok") {
		msg := strings.TrimSpace(res.Message)
		if msg == "" {
			msg = lastLine(errb.String())
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return res, errors.New(msg)
	}
	return res, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- the plugin directory (for the UI's search) ----

// CatalogPlugin is a plugin listed in Anthropic's directory.
type CatalogPlugin struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

const (
	catalogTTL   = 6 * time.Hour
	catalogRetry = time.Minute
)

func (m *Manager) catalogPath() string { return filepath.Join(m.cfg.DataDir, "plugin-catalog.json") }

// PluginCatalog lists the plugins of Anthropic's directory. It is fetched
// with the agent image's Claude Code in a throwaway container and cached
// for a few hours (a stale list is used when refreshing fails).
func (m *Manager) PluginCatalog() ([]CatalogPlugin, error) {
	m.catMu.Lock()
	defer m.catMu.Unlock()
	if m.catalog == nil {
		if b, err := os.ReadFile(m.catalogPath()); err == nil {
			var list []CatalogPlugin
			if json.Unmarshal(b, &list) == nil {
				m.catalog = list
				if fi, err := os.Stat(m.catalogPath()); err == nil {
					m.catAt = fi.ModTime()
				}
			}
		}
	}
	if m.catalog != nil && time.Since(m.catAt) < catalogTTL {
		return m.catalog, nil
	}
	// After a failure, wait a little before starting another container
	// (the UI searches as the user types).
	list, err := m.catalog, m.catErr
	if err == nil || time.Since(m.catErrAt) >= catalogRetry {
		list, err = m.fetchCatalog()
		m.catErr, m.catErrAt = err, time.Now()
	}
	if err != nil {
		if m.catalog != nil {
			log.Printf("plugin catalog: %v (using the cached one)", err)
			return m.catalog, nil
		}
		return nil, err
	}
	m.catalog, m.catAt = list, time.Now()
	if b, err := json.Marshal(list); err == nil {
		tmp := m.catalogPath() + ".collie-tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, m.catalogPath())
		}
	}
	return list, nil
}

func (m *Manager) fetchCatalog() ([]CatalogPlugin, error) {
	if !m.available {
		return nil, ErrUnavailable
	}
	if !m.Status().Ready {
		return nil, errors.New("the agent image is not built yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The network of a project with internet access.
	args := append([]string{"run", "--rm"}, m.networkArgs(model.Project{}, "")...)
	args = append(args, "--no-hosts", "--cap-drop", "all", "--security-opt", "no-new-privileges",
		m.tag, "claude", "plugin", "list", "--available", "--json")
	out, err := m.podman(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Available []struct {
			PluginID    string `json:"pluginId"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"available"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("plugin list: %v", err)
	}
	list := make([]CatalogPlugin, 0, len(raw.Available))
	for _, a := range raw.Available {
		if pluginIDRe.MatchString(a.PluginID) {
			list = append(list, CatalogPlugin{ID: a.PluginID, Name: a.Name, Description: a.Description})
		}
	}
	return list, nil
}

// SearchCatalog returns up to limit plugins matching q: name matches
// before description matches, otherwise in the directory's order.
func SearchCatalog(list []CatalogPlugin, q string, limit int) []CatalogPlugin {
	q = strings.ToLower(strings.TrimSpace(q))
	out := []CatalogPlugin{}
	if q == "" {
		return append(out, list[:min(limit, len(list))]...)
	}
	var prefix, name, desc []CatalogPlugin
	for _, c := range list {
		n := strings.ToLower(c.Name)
		switch {
		case strings.HasPrefix(n, q):
			prefix = append(prefix, c)
		case strings.Contains(n, q):
			name = append(name, c)
		case strings.Contains(strings.ToLower(c.Description), q):
			desc = append(desc, c)
		}
	}
	for _, part := range [][]CatalogPlugin{prefix, name, desc} {
		for _, c := range part {
			if len(out) == limit {
				return out
			}
			out = append(out, c)
		}
	}
	return out
}
