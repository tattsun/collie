package agent

import (
	"strings"
	"testing"
)

func TestCleanCommand(t *testing.T) {
	cases := map[string]string{
		`/bin/bash -lc 'touch a.txt'`:        `touch a.txt`,
		`bash -lc 'echo '\''hi'\'''`:         `echo 'hi'`,
		`/usr/bin/zsh -c "ls -la"`:           `ls -la`,
		`python3 x.py`:                       `python3 x.py`,
		`/bin/bash -lc "printf 'a\\n' >> f"`: `printf 'a\n' >> f`,
	}
	for in, want := range cases {
		if got := cleanCommand(in); got != want {
			t.Errorf("cleanCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexDiff(t *testing.T) {
	var files []string
	d := codexDiff([]any{
		map[string]any{"path": "new.txt", "kind": map[string]any{"type": "add"}, "diff": "hello\nworld\n"},
		map[string]any{"path": "a.go", "kind": map[string]any{"type": "update"}, "diff": "@@ -1 +1 @@\n-x\n+y\n"},
	}, &files)
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	for _, want := range []string{"--- /dev/null", "+++ b/new.txt", "+hello", "+world", "--- a/a.go", "-x\n+y"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("a", maxOutput*2)
	c := clip(s)
	if len(c) > maxOutput+100 || !strings.Contains(c, "omitted") {
		t.Fatalf("clip produced %d bytes", len(c))
	}
	if clip("short") != "short" {
		t.Fatal("short strings must be untouched")
	}
}
