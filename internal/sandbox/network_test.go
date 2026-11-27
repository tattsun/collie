package sandbox

import (
	"slices"
	"testing"

	"github.com/tattsun/collie/internal/egress"
	"github.com/tattsun/collie/internal/model"
)

func hasSeq(args []string, want ...string) bool {
	for i := range args {
		if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func TestNetworkRunArgs(t *testing.T) {
	m := newTestManager(t)
	m.egress = egress.NewProxy()
	open := model.Project{ID: "p1", Path: "/x/p", Runtime: "container"}
	listed := open
	listed.Network = model.NetAllowlist
	listed.NetAllow = []string{"github.com"}

	plain := m.runArgs(Spec{Project: open, UserID: "u1"}, nil)
	if !hasSeq(plain, "--network", "pasta:--map-host-loopback,none,--map-guest-addr,none") || !slices.Equal(plain[len(plain)-2:], []string{"sleep", "infinity"}) {
		t.Fatalf("a project on the internet changed: %q", plain)
	}
	for _, a := range plain {
		if a == "--network=none" || a == "none" || a == "COLLIE_NETWORK="+model.NetAllowlist {
			t.Fatalf("a project on the internet got the allowlist's %q", a)
		}
	}

	args := m.runArgs(Spec{Project: listed, UserID: "u1"}, nil)
	for _, want := range [][]string{
		{"--network", "none"},
		{"--label", "collie.network=allowlist"},
		{"-v", m.netDir("p1", "u1") + ":" + NetRunDir + ":ro"},
		{"-e", "HTTPS_PROXY=http://" + ProxyAddr},
		{"-e", "https_proxy=http://" + ProxyAddr},
		{"-e", "NO_PROXY=localhost,127.0.0.1,::1"},
		{m.tag, CollieBinPath, "net-bridge", NetRunDir + "/proxy.sock", ProxyAddr},
	} {
		if !hasSeq(args, want...) {
			t.Errorf("allowlisted container lacks %q", want)
		}
	}
	if slices.ContainsFunc(args, func(a string) bool { return len(a) > 5 && a[:5] == "pasta" }) {
		t.Error("allowlisted container still has pasta networking")
	}
	// The list itself is looked up live; only the mode re-creates containers.
	other := listed
	other.NetAllow = []string{"pypi.org"}
	if configHash(m.runArgs(Spec{Project: other, UserID: "u1"}, nil)) != configHash(args) {
		t.Error("changing the list would re-create the containers")
	}
	if configHash(plain) == configHash(args) {
		t.Error("switching to an allowlist would not re-create the container")
	}
	// A host-run project never gets one, whatever is stored.
	host := listed
	host.Runtime = "host"
	if host.Allowlisted() || networkMode(host) != "" {
		t.Error("a host-run project counts as allowlisted")
	}
}

func TestNetRulesFollowTheProject(t *testing.T) {
	m := newTestManager(t)
	m.egress = egress.NewProxy()
	projects := map[string]model.Project{
		"listed": {ID: "listed", Runtime: "container", Network: model.NetAllowlist, NetAllow: []string{"github.com"}},
		"open":   {ID: "open", Runtime: "container"},
		"host":   {ID: "host", Runtime: "host", Network: model.NetAllowlist, NetAllow: []string{"github.com"}},
	}
	m.SetNetRules(func(id string) (model.Project, bool) { p, ok := projects[id]; return p, ok })
	if l, ok := m.egress.Rules("listed"); !ok || !slices.Equal(l, []string{"github.com"}) {
		t.Errorf("listed project: %q %v", l, ok)
	}
	for _, id := range []string{"open", "host", "gone"} {
		if _, ok := m.egress.Rules(id); ok {
			t.Errorf("%s has an allowlist in the proxy", id)
		}
	}
}
