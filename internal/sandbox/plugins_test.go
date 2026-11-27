package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestPluginSetValidate(t *testing.T) {
	good := PluginSet{
		EnabledPlugins: map[string]bool{"frontend-design@anthropic-plugin-directory": true, "x.y_z@team-mkt": false},
		ExtraKnownMarketplaces: map[string]Marketplace{
			"team-mkt": {Source: MarketplaceSource{Source: "github", Repo: "org/plugins"}},
			"git-mkt":  {Source: MarketplaceSource{Source: "git", URL: "https://example.com/p.git"}},
			"ssh-mkt":  {Source: MarketplaceSource{Source: "git", URL: "git@github.com:org/p.git"}},
			"url-mkt":  {Source: MarketplaceSource{Source: "url", URL: "https://example.com/marketplace.json"}},
		},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	bad := []PluginSet{
		{EnabledPlugins: map[string]bool{"no-marketplace": true}},
		{EnabledPlugins: map[string]bool{"-flag@mkt": true}},
		{EnabledPlugins: map[string]bool{"a@-flag": true}},
		{EnabledPlugins: map[string]bool{"a b@mkt": true}},
		{ExtraKnownMarketplaces: map[string]Marketplace{"m": {Source: MarketplaceSource{Source: "github", Repo: "-x/y"}}}},
		{ExtraKnownMarketplaces: map[string]Marketplace{"m": {Source: MarketplaceSource{Source: "git", URL: "--upload-pack=x"}}}},
		{ExtraKnownMarketplaces: map[string]Marketplace{"m": {Source: MarketplaceSource{Source: "directory", Path: "/tmp/m"}}}},
		{ExtraKnownMarketplaces: map[string]Marketplace{BuiltinMarketplace: {Source: MarketplaceSource{Source: "github", Repo: "a/b"}}}},
		{ExtraKnownMarketplaces: map[string]Marketplace{"../x": {Source: MarketplaceSource{Source: "github", Repo: "a/b"}}}},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad set %d accepted: %+v", i, s)
		}
	}
}

func TestMergePluginsLaterLevelWins(t *testing.T) {
	eff, mkts := MergePlugins([]Level{
		{LevelGlobal, PluginSet{EnabledPlugins: map[string]bool{"a@m": true, "b@m": true}}},
		{LevelGroup, PluginSet{EnabledPlugins: map[string]bool{"c@m": true},
			ExtraKnownMarketplaces: map[string]Marketplace{"m": {Source: MarketplaceSource{Source: "github", Repo: "g/old"}}}}},
		{LevelPersonal, PluginSet{EnabledPlugins: map[string]bool{"b@m": false}}},
		{LevelProject, PluginSet{ExtraKnownMarketplaces: map[string]Marketplace{"m": {Source: MarketplaceSource{Source: "github", Repo: "p/new"}}}}},
		{LevelRepo, PluginSet{EnabledPlugins: map[string]bool{"a@m": true}}},
	})
	want := []EffectivePlugin{
		{ID: "a@m", Enabled: true, From: LevelRepo},
		{ID: "b@m", Enabled: false, From: LevelPersonal},
		{ID: "c@m", Enabled: true, From: LevelGroup},
	}
	if !slices.Equal(eff, want) {
		t.Fatalf("merged %+v, want %+v", eff, want)
	}
	if mkts["m"].Repo != "p/new" {
		t.Fatalf("marketplace m = %+v, want the project's", mkts["m"])
	}
}

func TestReadRepoPlugins(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{
	  "permissions": {"allow": ["Bash(ls)"]},
	  "enabledPlugins": {"a@m": true, "b@m": true, "bad id@m": true, "weird@m": ["x"]},
	  "extraKnownMarketplaces": {
	    "m": {"source": {"source": "directory", "path": "./plugins"}},
	    "evil": {"source": {"source": "git", "url": "-oProxyCommand=x"}}
	  }
	}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(`{"enabledPlugins": {"b@m": false}}`), 0o644)
	s, errs := readRepoPlugins(dir)
	if len(s.EnabledPlugins) != 2 || !s.EnabledPlugins["a@m"] || s.EnabledPlugins["b@m"] {
		t.Fatalf("plugins %v, want a@m on and b@m off (local wins), others skipped", s.EnabledPlugins)
	}
	if got := s.ExtraKnownMarketplaces["m"].Source.Path; got != filepath.Join(dir, "plugins") {
		t.Fatalf("directory marketplace path %q, want it resolved against the repository", got)
	}
	if _, ok := s.ExtraKnownMarketplaces["evil"]; ok {
		t.Fatal("a source that looks like a flag was kept")
	}
	if len(errs) != 2 {
		t.Fatalf("problems %v, want the invalid id and the evil marketplace", errs)
	}
}

func TestReadRepoPluginsDoesNotFollowSymlinksOut(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "settings.json"), []byte(`{"enabledPlugins": {"a@m": true}}`), 0o644)
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".claude")); err != nil {
		t.Skip(err)
	}
	if s, _ := readRepoPlugins(dir); len(s.EnabledPlugins) != 0 {
		t.Fatalf("read settings through a symlink out of the repository: %v", s.EnabledPlugins)
	}
}

func TestPlanPlugins(t *testing.T) {
	h := homePlugins{
		installed: map[string]bool{"keep@m": true, "manual@m": true, "old@m": true, "manual-unwanted@m": true},
		managed:   map[string]bool{"keep@m": true, "old@m": true, "gone@m": true},
	}
	p := planPlugins([]string{"keep@m", "manual@m", "new@m", "gone@m"}, h)
	if !slices.Equal(p.install, []string{"gone@m", "new@m"}) {
		t.Errorf("install %v, want the missing ones (incl. one removed by hand)", p.install)
	}
	if !slices.Equal(p.uninstall, []string{"old@m"}) {
		t.Errorf("uninstall %v, want only Collie's unwanted one (never manual installs)", p.uninstall)
	}
	if len(p.managed) != 1 || !p.managed["keep@m"] {
		t.Errorf("managed %v, want keep@m (manual@m stays the user's)", p.managed)
	}
}

func TestReadHomePlugins(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "plugins"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), []byte(`{"version":2,"plugins":{
	  "u@m": [{"scope":"user","version":"1"}],
	  "p@m": [{"scope":"project","projectPath":"/x"}]
	}}`), 0o600)
	os.WriteFile(filepath.Join(home, ".claude", "plugins", "known_marketplaces.json"), []byte(`{"m":{"source":{"source":"github","repo":"a/b"}}}`), 0o600)
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeManaged(root, map[string]bool{"u@m": true}); err != nil {
		t.Fatal(err)
	}
	h := readHomePlugins(root)
	if !h.installed["u@m"] || h.installed["p@m"] || !h.markets["m"] || !h.managed["u@m"] {
		t.Fatalf("read %+v", h)
	}
}

func TestPluginLevels(t *testing.T) {
	m := newTestManager(t)
	write := func(path, content string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(m.GlobalPluginsPath(), `{"enabledPlugins":{"g@m":true,"broken":true}}`)
	write(m.GroupPluginsPath("g1"), `{"enabledPlugins":{"grp@m":true}}`)
	write(m.PersonalPluginsPath("u1"), `{"enabledPlugins":{"g@m":false}}`)
	write(m.ProjectPluginsPath("p1"), `{"enabledPlugins":{"proj@m":true}}`)
	repo := t.TempDir()
	write(filepath.Join(repo, ".claude", "settings.json"), `{"enabledPlugins":{"repo@m":true}}`)

	names := func(levels []Level) (out []string) {
		for _, l := range levels {
			out = append(out, l.Name)
		}
		return out
	}
	levels, errs := m.PluginLevels(model.Project{ID: "p1", OwnerGroup: "g1"}, "u1", repo)
	if !slices.Equal(names(levels), []string{LevelGlobal, LevelGroup, LevelPersonal, LevelProject, LevelRepo}) {
		t.Fatalf("levels %v", names(levels))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken") {
		t.Fatalf("problems %v, want the invalid global entry", errs)
	}
	eff, _ := MergePlugins(levels)
	var on []string
	for _, e := range eff {
		if e.Enabled {
			on = append(on, e.ID)
		}
	}
	if !slices.Equal(on, []string{"grp@m", "proj@m", "repo@m"}) {
		t.Fatalf("enabled %v", on)
	}
	// A personal project has no group level, and "" leaves the repository out.
	levels, _ = m.PluginLevels(model.Project{ID: "p2", OwnerUser: "u1"}, "u1", "")
	if !slices.Equal(names(levels), []string{LevelGlobal, LevelPersonal, LevelProject}) {
		t.Fatalf("levels %v", names(levels))
	}
}

func TestWritePluginSetValidates(t *testing.T) {
	m := newTestManager(t)
	if err := WritePluginSet(m.ProjectPluginsPath("p1"), PluginSet{EnabledPlugins: map[string]bool{"nope": true}}); err == nil {
		t.Fatal("invalid set written")
	}
	want := PluginSet{EnabledPlugins: map[string]bool{"a@m": true}}
	if err := WritePluginSet(m.GroupPluginsPath("g1"), want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPluginSet(m.GroupPluginsPath("g1"))
	if err != nil || !got.EnabledPlugins["a@m"] {
		t.Fatalf("read back %+v, %v", got, err)
	}
	m.RemoveGroup("g1")
	if _, err := os.Stat(m.GroupPluginsPath("g1")); !os.IsNotExist(err) {
		t.Fatal("group plugins left behind")
	}
}

func TestSearchCatalog(t *testing.T) {
	list := []CatalogPlugin{
		{ID: "superpowers@d", Name: "superpowers", Description: "skills library"},
		{ID: "frontend-design@d", Name: "frontend-design", Description: "UI design"},
		{ID: "design@d", Name: "design", Description: "design workflows"},
		{ID: "figma@d", Name: "figma", Description: "Figma design platform"},
	}
	got := SearchCatalog(list, "Design", 10)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Name)
	}
	if !slices.Equal(ids, []string{"design", "frontend-design", "figma"}) {
		t.Fatalf("search order %v", ids)
	}
	if n := len(SearchCatalog(list, "", 2)); n != 2 {
		t.Fatalf("empty query returned %d, want the first 2", n)
	}
}
