package gitutil

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// inProcess is a Helper that serves its requests in this process, as
// `collie git-helper` does in a container.
func inProcess(t *testing.T) Helper {
	return Helper{Run: func(_ time.Duration, req []byte) ([]byte, error) {
		var out bytes.Buffer
		if code := ServeHelper(bytes.NewReader(req), &out); code != 0 {
			t.Fatalf("git-helper exited with %d", code)
		}
		return out.Bytes(), nil
	}}
}

func TestHelperRoundTrip(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	h := inProcess(t)
	plain := t.TempDir()
	if err := h.CheckWorktrees(plain); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("not a repository: %v", err)
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	if err := h.CheckWorktrees(repo); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("files but no commits: %v", err)
	}
	if _, err := h.NewWorktree(repo, filepath.Join(t.TempDir(), "wt"), "collie/x", Author{Name: "n", Email: "n@e"}); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("worktree over uncommitted files: %v", err)
	}
	os.Remove(filepath.Join(repo, "a.txt"))

	// An empty repository gets an initial commit to branch off.
	wt := filepath.Join(t.TempDir(), "wt")
	base, err := h.NewWorktree(repo, wt, "collie/x", Author{Name: "n", Email: "n@e"})
	if err != nil || base == "" || base != HeadCommit(repo) || base != HeadCommit(wt) {
		t.Fatalf("NewWorktree = %q, %v (head %s)", base, err, HeadCommit(repo))
	}
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("<b>hi</b>\n"), 0o644)
	ch, err := h.Diff(wt, base)
	if err != nil || !ch.Repo || len(ch.Files) != 1 || ch.Files[0].Path != "new.txt" || !strings.Contains(ch.Files[0].Diff, "+<b>hi</b>") {
		t.Fatalf("Diff = %+v, %v", ch, err)
	}
	if ch, err := h.Diff(plain, ""); err != nil || ch.Repo || ch.Files == nil {
		t.Fatalf("Diff of a plain folder = %+v, %v", ch, err)
	}
	if err := h.RemoveWorktree(repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
}

// What comes back from a container is the agent's to make up.
func TestHelperDistrustsAnswers(t *testing.T) {
	answer := func(s string) Helper {
		return Helper{Run: func(time.Duration, []byte) ([]byte, error) { return []byte(s), nil }}
	}
	if _, err := answer(`{"base":"--output=/home/collie/x"}`).NewWorktree("/r", "/w", "b", Author{}); err == nil {
		t.Error("an option passed for a base commit")
	}
	if _, err := answer(`{"base":"`+strings.Repeat("a", 40)+`"}`).NewWorktree("/r", "/w", "b", Author{}); err != nil {
		t.Error(err)
	}
	if _, err := answer(`not json`).Diff("/r", ""); err == nil {
		t.Error("garbage taken for changes")
	}
}

// An untracked symlink shows as its target: following it would show the
// diff's reader files from outside the repository.
func TestDiffDoesNotFollowUntrackedSymlinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("top secret\n"), 0o600)
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	commitFile(t, repo, "a.txt")
	os.Symlink(outside, filepath.Join(repo, "abs"))
	os.Symlink("../../"+filepath.Base(filepath.Dir(outside))+"/secret", filepath.Join(repo, "rel"))
	ch, err := Diff(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Files) != 2 {
		t.Fatalf("files = %+v", ch.Files)
	}
	for _, f := range ch.Files {
		if strings.Contains(f.Diff, "top secret") {
			t.Errorf("%s shows the file it points to:\n%s", f.Path, f.Diff)
		}
	}
	if d := ch.Files[0].Diff; !strings.Contains(d, "+"+outside) {
		t.Errorf("abs shows %q", d)
	}

	// Nor does reading one hang on a FIFO swapped in after git listed it.
	if err := syscall.Mkfifo(filepath.Join(repo, "fifo"), 0o644); err != nil {
		t.Skip(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan struct{})
	go func() {
		untrackedDiff(root, "fifo")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO hangs")
	}
}

func TestHeadBranch(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "trunk")
	if b := HeadBranch(repo); b != "trunk" {
		t.Errorf("HeadBranch = %q", b)
	}
	commitFile(t, repo, "a.txt")
	git(t, repo, "checkout", "-q", "--detach")
	if b := HeadBranch(repo); b != "HEAD" {
		t.Errorf("detached: %q", b)
	}
	// A HEAD that points elsewhere is not read.
	outside := filepath.Join(t.TempDir(), "x")
	os.WriteFile(outside, []byte("ref: refs/heads/secret\n"), 0o644)
	os.Remove(filepath.Join(repo, ".git", "HEAD"))
	os.Symlink(outside, filepath.Join(repo, ".git", "HEAD"))
	if b := HeadBranch(repo); b != "" {
		t.Errorf("through a symlink: %q", b)
	}
	if b := HeadBranch(t.TempDir()); b != "" {
		t.Errorf("no repository: %q", b)
	}
}
