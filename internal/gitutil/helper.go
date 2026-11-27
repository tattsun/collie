package gitutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Helper runs Ops through `collie git-helper` (ServeHelper) in a project's
// container. Run starts it there with req on its stdin and returns what it
// printed, giving up after timeout. Everything that comes back was made in
// the container, so it is only ever used as data.
type Helper struct {
	Run func(timeout time.Duration, req []byte) ([]byte, error)
}

type helperReq struct {
	Op     string `json:"op"`
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Author Author `json:"author"`
}

type helperResp struct {
	Error   string   `json:"error,omitempty"`
	Code    string   `json:"code,omitempty"` // for errors callers tell apart
	Base    string   `json:"base,omitempty"`
	Changes *Changes `json:"changes,omitempty"`
}

var errCodes = map[string]error{"no-repo": ErrNoRepo, "no-commits": ErrNoCommits}

func (h Helper) call(timeout time.Duration, req helperReq) (helperResp, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return helperResp{}, err
	}
	out, err := h.Run(timeout, b)
	if err != nil {
		return helperResp{}, err
	}
	var resp helperResp
	if err := json.Unmarshal(out, &resp); err != nil {
		return helperResp{}, errors.New("git in the project's container gave no usable answer")
	}
	if e, ok := errCodes[resp.Code]; ok {
		return resp, e
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func (h Helper) CheckWorktrees(repo string) error {
	_, err := h.call(time.Minute, helperReq{Op: "check-worktrees", Repo: repo})
	return err
}

func (h Helper) NewWorktree(repo, path, branch string, author Author) (string, error) {
	resp, err := h.call(5*time.Minute, helperReq{Op: "new-worktree", Repo: repo, Path: path, Branch: branch, Author: author})
	if err != nil {
		return "", err
	}
	// It becomes an argument of git commands later.
	if resp.Base != "" && !isCommitID(resp.Base) {
		return "", fmt.Errorf("git in the project's container reported %q as the base commit", resp.Base)
	}
	return resp.Base, nil
}

func (h Helper) RemoveWorktree(repo, path string) error {
	_, err := h.call(2*time.Minute, helperReq{Op: "remove-worktree", Repo: repo, Path: path})
	return err
}

func (h Helper) Diff(dir, base string) (*Changes, error) {
	resp, err := h.call(2*time.Minute, helperReq{Op: "diff", Repo: dir, Base: base})
	if err != nil {
		return nil, err
	}
	if resp.Changes == nil {
		return nil, errors.New("git in the project's container reported no changes")
	}
	if resp.Changes.Files == nil {
		resp.Changes.Files = []FileChange{}
	}
	return resp.Changes, nil
}

func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ServeHelper answers one request of a Helper, read from r, on w with git
// on this machine. It is `collie git-helper`, run in a project's container.
func ServeHelper(r io.Reader, w io.Writer) int {
	var req helperReq
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "collie git-helper:", err)
		return 2
	}
	var (
		l    Local
		resp helperResp
		err  error
	)
	switch req.Op {
	case "check-worktrees":
		err = l.CheckWorktrees(req.Repo)
	case "new-worktree":
		resp.Base, err = l.NewWorktree(req.Repo, req.Path, req.Branch, req.Author)
	case "remove-worktree":
		err = l.RemoveWorktree(req.Repo, req.Path)
	case "diff":
		resp.Changes, err = l.Diff(req.Repo, req.Base)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	if err != nil {
		resp.Error = err.Error()
		for code, e := range errCodes {
			if errors.Is(err, e) {
				resp.Code = code
			}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // diffs are full of < and >
	if err := enc.Encode(resp); err != nil {
		fmt.Fprintln(os.Stderr, "collie git-helper:", err)
		return 1
	}
	return 0
}
