package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestRemoteRunArgs(t *testing.T) {
	m := newTestManager(t)
	sp := Spec{Project: model.Project{ID: "p1", Path: "/x/p"}, UserID: "u1", CollieRun: "/host/run", CollieBin: "/host/collie"}
	local := m.runArgs(sp, nil)
	if !hasSeq(local, "-v", "/host/run:"+CollieRunDir) || !hasSeq(local, "-v", "/host/collie:"+CollieBinPath+":ro") {
		t.Fatalf("local container lacks the tools socket or binary: %q", local)
	}
	m.remote = true
	remote := m.runArgs(sp, nil)
	if !hasSeq(remote, "--tmpfs", CollieRunDir) {
		t.Errorf("remote container has no tmpfs for the relay's socket: %q", remote)
	}
	for _, a := range remote {
		if strings.Contains(a, "/host/") {
			t.Errorf("remote container mounts a host socket or binary: %s", a)
		}
	}
}

func TestRemoteRefusesNested(t *testing.T) {
	m := newTestManager(t)
	m.available, m.remote = true, true
	err := m.Ensure(Spec{Project: model.Project{ID: "p1", Path: "/x/p", Nested: true}, UserID: "u1"}, false)
	if !errors.Is(err, errNestedRemote) {
		t.Fatalf("nested project on a remote Podman: %v", err)
	}
}

func TestRemoteHostInfo(t *testing.T) {
	m := newTestManager(t)
	m.remote = true
	for _, c := range []struct {
		h      hostInfo
		limits bool
	}{
		{hostInfo{cgroupManager: "systemd", cgroupVersion: "v2", selinux: true}, true},
		{hostInfo{cgroupManager: "cgroupfs", cgroupVersion: "v2"}, false},
		{hostInfo{cgroupManager: "systemd", cgroupVersion: "v1"}, false},
	} {
		m.applyHostInfo(c.h)
		if m.LimitsSupported() != c.limits || m.selinux != c.h.selinux {
			t.Errorf("%+v: limits %v selinux %v", c.h, m.LimitsSupported(), m.selinux)
		}
	}
}

func TestParseHostInfo(t *testing.T) {
	h, err := parseHostInfo([]byte(`{"host":{"cgroupManager":"systemd","cgroupVersion":"v2","security":{"selinuxEnabled":true},
		"idMappings":{"uidmap":[{"container_id":0,"host_id":501,"size":1},{"container_id":1,"host_id":100000,"size":65536}],
		"gidmap":[{"container_id":0,"host_id":1000,"size":1}]}}}`))
	if err != nil || h != (hostInfo{cgroupManager: "systemd", cgroupVersion: "v2", selinux: true, user: "501:1000"}) {
		t.Fatalf("%+v %v", h, err)
	}
	m := newTestManager(t)
	m.remote = true
	m.applyHostInfo(h)
	if m.userSpec() != "501:1000" {
		t.Errorf("a remote Podman's custom images run as %s, not its VM's user", m.userSpec())
	}
}
