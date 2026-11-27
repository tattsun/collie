package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The agent image is built here in two parts: a base with the slow,
// rarely changing system packages (image/base/Containerfile), and the
// agents on top (image/Containerfile). The base comes ready-made from
// GHCR so a server doesn't have to build it.

// BaseImage is the base this build uses, pinned by digest
// (ghcr.io/…/collie-agent-base@sha256:…). The release workflow stamps it
// in (tools/ldflags.sh), so each signed release decides which base its
// servers run, and the registry can't hand them another. Builds from
// source leave it empty and use the base published for their
// base/Containerfile (BaseRepo:<hash>), or build that file here.
var BaseImage = ""

// BaseRepo is where the release workflow publishes the base.
var BaseRepo = "ghcr.io/tattsun/collie-agent-base"

const (
	baseFile      = "image/base/Containerfile"
	localBaseRepo = "localhost/collie-agent-base"
)

// BaseHash identifies the base definition: the first 12 hex digits of the
// SHA-256 of base/Containerfile, i.e. what
// `sha256sum internal/sandbox/image/base/Containerfile | cut -c1-12` prints
// in the release workflow.
func BaseHash() string {
	b, err := imageFS.ReadFile(baseFile)
	if err != nil {
		panic(err) // embedded; a test makes sure it is there
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// baseRefs returns the references the base may go by on this server: the
// pinned digest for a release, else the published tag and a local build.
func baseRefs() []string {
	if BaseImage != "" {
		return []string{BaseImage}
	}
	return []string{BaseRepo + ":" + BaseHash(), localBaseRepo + ":" + BaseHash()}
}

// prepareBase makes sure the base exists and returns its reference. dir
// holds the extracted image definition (with base/ inside).
func (m *Manager) prepareBase(ctx context.Context, dir string, w io.Writer) (string, error) {
	refs := baseRefs()
	for _, ref := range refs {
		if _, err := m.podman(ctx, "image", "exists", ref); err == nil {
			return ref, nil
		}
	}
	fmt.Fprintf(w, "pulling the base image %s …\n", refs[0])
	err := m.stream(ctx, dir, w, "pull", refs[0])
	if err == nil {
		return refs[0], nil
	}
	if BaseImage != "" {
		return "", fmt.Errorf("pulling the base image %s failed: %w", refs[0], err)
	}
	fmt.Fprintf(w, "no published base image for this definition; building it here (takes a few minutes) …\n")
	if err := m.stream(ctx, dir+"/base", w, "build", "-t", refs[1], "-f", "Containerfile", "."); err != nil {
		return "", fmt.Errorf("base image build failed: %w", err)
	}
	return refs[1], nil
}

// stream runs podman in dir with its output going to w. Builds' RUN steps
// get the agents' network (see buildEnv).
func (m *Manager) stream(ctx context.Context, dir string, w io.Writer, args ...string) error {
	conf, err := os.MkdirTemp("", "collie-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(conf)
	cmd := exec.CommandContext(ctx, m.cfg.Podman, append(append([]string(nil), m.global...), args...)...)
	cmd.Dir = dir
	cmd.Env = buildEnv(conf)
	cmd.Stdout, cmd.Stderr = w, w
	// Out of time: let podman stop its build and clean up first.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	return cmd.Run()
}

// pruneBases removes base images other than the ones this version uses.
// Pulled by digest they keep a name, so the dangling-image prune never
// gets them. podman refuses while an agent image built on one (or a
// container) is still around; a later prune gets those.
func (m *Manager) pruneBases(ctx context.Context) {
	out, err := m.podman(ctx, "images", "--format", "json", "--filter", "label=io.collie.agent=1")
	if err != nil {
		return
	}
	var imgs []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if json.Unmarshal([]byte(out), &imgs) != nil {
		return
	}
	keep := map[string]bool{}
	for _, r := range baseRefs() {
		keep[r] = true
	}
	for _, im := range imgs {
		old, current := false, false
		for _, n := range im.Names {
			if keep[n] {
				current = true
			} else if isBaseName(n) {
				old = true
			}
		}
		if old && !current {
			if _, err := m.podman(ctx, "rmi", im.ID); err == nil {
				log.Printf("removed old agent base image %s", strings.Join(im.Names, ", "))
			}
		}
	}
}

func isBaseName(n string) bool {
	return strings.HasPrefix(n, BaseRepo+":") || strings.HasPrefix(n, BaseRepo+"@") ||
		strings.HasPrefix(n, localBaseRepo+":")
}
