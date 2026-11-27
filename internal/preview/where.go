package preview

import (
	"path/filepath"
	"strconv"
	"strings"
)

// Which session a server belongs to is told by where it works: the
// working directory of the process that listens, or failing that of its
// parents (a server started from the agent's shell in its worktree, which
// then changed directory). Sessions work in folders of their own (their
// worktrees) or share the project's folder.

// maxAncestors bounds how far up the process tree a server's folder is
// looked for.
const maxAncestors = 16

// ancestry lists the working directories of a process and its parents,
// nearest first, without repeats, up to (and not including) stop: on the
// host that is Collie itself, whose folder says nothing about whose
// server it is.
func ancestry(pid, stop int, parent func(int) int, cwd func(int) string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < maxAncestors && pid > 1 && pid != stop; i++ {
		if d := cwd(pid); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
		pid = parent(pid)
	}
	return out
}

// parseStatPPid reads the parent's pid from /proc/<pid>/stat (the command
// name, in parentheses, may itself contain spaces and parentheses).
func parseStatPPid(stat string) int {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// cleanCwd keeps a working directory only if it still exists as a path.
func cleanCwd(d string) string {
	if d == "" || !filepath.IsAbs(d) || strings.HasSuffix(d, " (deleted)") {
		return ""
	}
	return filepath.Clean(d)
}

// parsePs reads `ps -A -o pid=,ppid=` (systems without /proc).
func parsePs(out string) map[int]int {
	m := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			m[pid] = ppid
		}
	}
	return m
}

// parseLsofCwd reads `lsof -a -d cwd -Fpn -p …`.
func parseLsofCwd(out string) map[int]string {
	m := map[int]string{}
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			if d := cleanCwd(line[1:]); d != "" && pid > 0 {
				m[pid] = d
			}
		}
	}
	return m
}

// Folder is where some sessions work: their own worktree, or the
// project's folder (which sessions without a worktree share, and which
// may have none).
type Folder struct {
	Path     string
	Sessions []string
}

// Where tells which folder a server works in: for the nearest of its
// directories that lies in any folder, the most specific folder holding
// it. ok is false when none does.
func Where(dirs []string, folders []Folder) (Folder, bool) {
	for _, d := range dirs {
		best := -1
		for i, f := range folders {
			if inside(d, f.Path) && (best < 0 || len(f.Path) > len(folders[best].Path)) {
				best = i
			}
		}
		if best >= 0 {
			return folders[best], true
		}
	}
	return Folder{}, false
}

func inside(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}
