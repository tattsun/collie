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

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitIn(t, dir, "", args...)
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
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

func TestInitialCommit(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	if HasCommits(repo) || HasFiles(repo) {
		t.Fatal("a fresh repository has neither commits nor files")
	}
	os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("x\n"), 0o644)
	git(t, repo, "add", "staged.txt")
	if !HasFiles(repo) {
		t.Fatal("a staged file counts")
	}
	if err := InitialCommit(repo, "Ann", "ann@example.com"); err != nil {
		t.Fatal(err)
	}
	if !HasCommits(repo) {
		t.Fatal("no commit")
	}
	log := func(format string) string {
		out, err := exec.Command("git", "-C", repo, "log", "-1", "--format="+format).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := log("%an <%ae> %s"); got != "Ann <ann@example.com> Initial commit" {
		t.Fatalf("commit = %q", got)
	}
	// Empty, and the staged file is still only staged.
	if out, _ := exec.Command("git", "-C", repo, "ls-tree", "-r", "HEAD").Output(); len(out) != 0 {
		t.Fatalf("tree = %q", out)
	}
	if out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); string(out) != "A  staged.txt\n" {
		t.Fatalf("status = %q", out)
	}
	// Someone else got there first: nothing changes.
	head := HeadCommit(repo)
	if err := InitialCommit(repo, "Bob", "bob@example.com"); err != nil || HeadCommit(repo) != head {
		t.Fatalf("second call: %v, head %s -> %s", err, head, HeadCommit(repo))
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(repo, wt, "collie/test", head); err != nil {
		t.Fatal(err)
	}
}

// Reading the working tree (the Changes tab) or removing a worktree runs
// nothing the repository's configuration names: in a group project it runs
// in a member's container on a repository another member can rig.
func TestReadsRunNothingFromTheRepository(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	marker := filepath.Join(t.TempDir(), "ran")
	cmd := "sh -c 'echo $0 >> " + marker + "'"
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "b.bin"), []byte("one\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(repo, wt, "collie/x", ""); err != nil {
		t.Fatal(err)
	}
	// A commit with a signature on the worktree's branch: listing it with
	// log.showSignature would run gpg.program.
	base := HeadCommit(repo)
	tree := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD^{tree}"))
	obj := "tree " + tree + "\nparent " + base + "\nauthor a <a@b> 0 +0000\ncommitter a <a@b> 0 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n -----END PGP SIGNATURE-----\n\nsigned\n"
	signed := strings.TrimSpace(gitIn(t, wt, obj, "hash-object", "-t", "commit", "-w", "--stdin"))
	git(t, wt, "update-ref", "HEAD", signed)
	gpg := filepath.Join(t.TempDir(), "gpg") // run as a program, not by a shell
	os.WriteFile(gpg, []byte("#!/bin/sh\necho gpg >> "+marker+"\n"), 0o755)
	for k, v := range map[string]string{
		"log.showSignature":       "true",
		"gpg.program":             gpg,
		"core.fsmonitor":          cmd + " fsmonitor",
		"diff.external":           cmd + " external",
		"diff.evil.textconv":      cmd + " textconv",
		"diff.evil.command":       cmd + " diffcmd",
		"filter.evil.clean":       cmd + " clean",
		"filter.evil.process":     cmd + " process",
		"filter.evil.required":    "true",
		"filter.a=b.clean":        cmd + " clean-eq",
		"filter.a=b.required":     "true",
		"core.hooksPath":          filepath.Join(repo, "hooks"),
		"status.submoduleSummary": "true",
		"submodule.recurse":       "true",
	} {
		git(t, repo, "config", k, v)
	}
	os.MkdirAll(filepath.Join(repo, "hooks"), 0o755)
	for _, h := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
		os.WriteFile(filepath.Join(repo, "hooks", h), []byte("#!/bin/sh\necho hook-"+h+" >> "+marker+"\n"), 0o755)
	}
	attrs := "a.txt filter=evil diff=evil\nb.bin filter=a=b diff=evil\n"
	os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte(attrs), 0o644)
	// Dirty both, so status and diff look at their contents.
	for _, d := range []string{repo, wt} {
		os.WriteFile(filepath.Join(d, "a.txt"), []byte("two\n"), 0o644)
		os.WriteFile(filepath.Join(d, "b.bin"), []byte("two\n"), 0o644)
		os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0o644)
	}
	if !HasFiles(repo) {
		t.Fatal("HasFiles = false")
	}
	ch, err := Diff(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Files) < 2 {
		t.Fatalf("Diff found %d files", len(ch.Files))
	}
	if ch, err := Diff(wt, base); err != nil {
		t.Fatal(err)
	} else if len(ch.Commits) != 1 {
		t.Fatalf("Diff listed commits %q", ch.Commits)
	}
	if err := CheckWorktrees(repo); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorktree(repo, wt); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("git ran what the repository names:\n%s", b)
	}
}
