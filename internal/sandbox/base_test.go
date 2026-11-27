package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release workflow names the published base after
// `sha256sum internal/sandbox/image/base/Containerfile | cut -c1-12`, so
// BaseHash must be exactly that, and base/ must hold nothing else that
// could change the image without changing the hash.
func TestBaseHashIsSha256sumOfTheContainerfile(t *testing.T) {
	b, err := os.ReadFile("image/base/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got, want := BaseHash(), hex.EncodeToString(sum[:])[:12]; got != want {
		t.Fatalf("BaseHash() = %s, want %s", got, want)
	}
	var files []string
	_ = fs.WalkDir(imageFS, "image/base", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 || files[0] != baseFile {
		t.Fatalf("image/base must contain only Containerfile, has %v", files)
	}
}

func TestBaseRefs(t *testing.T) {
	defer func(v string) { BaseImage = v }(BaseImage)

	BaseImage = ""
	refs := baseRefs()
	if len(refs) != 2 || refs[0] != BaseRepo+":"+BaseHash() || refs[1] != localBaseRepo+":"+BaseHash() {
		t.Fatalf("from source: %v", refs)
	}

	BaseImage = BaseRepo + "@sha256:" + strings.Repeat("a", 64)
	if refs := baseRefs(); len(refs) != 1 || refs[0] != BaseImage {
		t.Fatalf("release: %v", refs)
	}
}

// A release pinning another base must not reuse the previous agent image.
func TestImageHashFollowsBaseImage(t *testing.T) {
	defer func(v string) { BaseImage = v }(BaseImage)
	BaseImage = ""
	src := imageHash()
	BaseImage = BaseRepo + "@sha256:" + strings.Repeat("a", 64)
	a := imageHash()
	BaseImage = BaseRepo + "@sha256:" + strings.Repeat("b", 64)
	b := imageHash()
	if src == a || a == b {
		t.Fatalf("image hash ignores the base: %s %s %s", src, a, b)
	}
}

func TestIsBaseName(t *testing.T) {
	for n, want := range map[string]bool{
		BaseRepo + ":0123456789ab":                      true,
		BaseRepo + "@sha256:" + strings.Repeat("a", 64): true,
		localBaseRepo + ":0123456789ab":                 true,
		"localhost/collie-agent:0123456789ab":           false,
		"localhost/collie-agent:latest":                 false,
		BaseRepo + "-other:1":                           false,
	} {
		if got := isBaseName(n); got != want {
			t.Errorf("isBaseName(%q) = %v, want %v", n, got, want)
		}
	}
}

func TestExtractImageKeepsLayout(t *testing.T) {
	dir := t.TempDir()
	if err := extractImage(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Containerfile", "collie-exec", "collie-kill", "base/Containerfile"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Containerfile"))
	if !strings.Contains(string(b), "FROM ${BASE}") {
		t.Errorf("the agent layer must build on the base passed in as BASE")
	}
}
