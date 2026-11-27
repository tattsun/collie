package agent

import (
	"encoding/base64"
	"fmt"
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

func TestAuthNotice(t *testing.T) {
	cases := map[string]string{
		"Not logged in · Please run /login":                                     "no usable account",
		"Failed to authenticate. API Error: 401 OAuth access token is invalid.": "token was rejected",
		"API Error: 529 Overloaded":                                             "",
		"":                                                                      "",
	}
	for in, want := range cases {
		got := authNotice(in)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("authNotice(%q) = %q, want it to mention %q", in, got, want)
		}
	}
}

type blobSink struct {
	capsSink
	blobs [][]byte
}

func (s *blobSink) Blob(b []byte) string {
	s.blobs = append(s.blobs, b)
	return fmt.Sprintf("img%d.png", len(s.blobs))
}

func TestToolImages(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))
	s := &blobSink{}
	got := toolImages(s, []any{
		map[string]any{"type": "text", "text": "Took the screenshot"},
		// Claude's tool_result form
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
		// MCP's form (Codex)
		map[string]any{"type": "image", "mimeType": "image/png", "data": png},
		map[string]any{"type": "image", "data": "!!not base64"},
	})
	if len(got) != 2 || len(s.blobs) != 2 || !strings.HasPrefix(string(s.blobs[0]), "\x89PNG") {
		t.Fatalf("toolImages = %v (%d blobs)", got, len(s.blobs))
	}
	if toolImages(s, "plain text output") != nil {
		t.Error("images found in a string")
	}
}
