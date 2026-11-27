package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWorktreeDiffAgainstBase(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "init")
	base := HeadCommit(repo)

	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(repo, wt, "collie/test", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(CommonDir(wt), ".git") {
		t.Fatalf("CommonDir = %q", CommonDir(wt))
	}
	// one committed change, one uncommitted, one untracked
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("committed\n"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-qm", "add b")
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("new\n"), 0o644)

	ch, err := Diff(wt, base)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ch.Files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != "M" || got["b.txt"] != "A" || got["c.txt"] != "?" {
		t.Fatalf("files = %v", got)
	}
	if len(ch.Commits) != 1 || !strings.Contains(ch.Commits[0], "add b") {
		t.Fatalf("commits = %v", ch.Commits)
	}
	if err := RemoveWorktree(repo, wt); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", name)
}

func TestFreshBase(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "main")
	commitFile(t, origin, "a")
	local := filepath.Join(t.TempDir(), "local")
	git(t, filepath.Dir(local), "clone", "-q", origin, local)
	old := HeadCommit(local)

	// no upstream movement: HEAD
	if got := FreshBase(local); got != old {
		t.Fatalf("unchanged: got %s want %s", got, old)
	}
	// upstream moves ahead: use its tip, leave local branch alone
	commitFile(t, origin, "b")
	tip := HeadCommit(origin)
	if got := FreshBase(local); got != tip {
		t.Fatalf("behind: got %s want %s", got, tip)
	}
	if HeadCommit(local) != old {
		t.Fatal("local branch was modified")
	}
	// local has its own commit and upstream moved: diverged, keep HEAD
	commitFile(t, local, "c")
	if got, want := FreshBase(local), HeadCommit(local); got != want {
		t.Fatalf("diverged: got %s want %s", got, want)
	}
}

func TestFreshBaseDefaultBranchWithoutUpstream(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "main")
	commitFile(t, origin, "a")
	local := filepath.Join(t.TempDir(), "local")
	git(t, filepath.Dir(local), "clone", "-q", origin, local)
	// old feature branch with no upstream, then main moves on
	git(t, local, "checkout", "-q", "-b", "feature")
	commitFile(t, origin, "b")
	tip := HeadCommit(origin)
	if got := FreshBase(local); got != tip {
		t.Fatalf("got %s want default branch tip %s", got, tip)
	}
	// feature gains its own commit: keep HEAD
	commitFile(t, local, "c")
	if got, want := FreshBase(local), HeadCommit(local); got != want {
		t.Fatalf("diverged: got %s want %s", got, want)
	}
}
