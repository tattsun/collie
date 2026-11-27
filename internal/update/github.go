package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// release is the part of a GitHub release the updater uses.
type release struct {
	Tag       string
	Notes     string
	URL       string
	Published time.Time
	assets    map[string]asset
}

type asset struct {
	url  string // API URL; served as the file with Accept: application/octet-stream
	size int64
}

type github struct {
	api, repo, token string
	hc               *http.Client
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func newGitHub(api, repo, token string) github {
	if api == "" {
		api = "https://api.github.com"
	}
	hc := &http.Client{
		Timeout: 10 * time.Minute,
		// Asset downloads redirect to a storage host: the token is for GitHub
		// only. (net/http drops it across domains already; this makes it
		// explicit.)
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if r.URL.Host != via[0].URL.Host {
				r.Header.Del("Authorization")
			}
			return nil
		},
	}
	return github{api: api, repo: repo, token: token, hc: hc}
}

func (g github) request(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "collie-updater")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	return g.hc.Do(req)
}

// latest returns the newest published release (GitHub leaves drafts and
// pre-releases out).
func (g github) latest(ctx context.Context) (release, error) {
	if !repoRe.MatchString(g.repo) {
		return release{}, fmt.Errorf("invalid repository %q (want owner/name)", g.repo)
	}
	resp, err := g.request(ctx, g.api+"/repos/"+g.repo+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return release{}, fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound && g.token == "":
		return release{}, errors.New("no release found (a private repository needs a GitHub token)")
	case resp.StatusCode == http.StatusNotFound:
		return release{}, errors.New("no release found")
	case resp.StatusCode == http.StatusUnauthorized:
		return release{}, errors.New("GitHub rejected the token")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return release{}, errors.New("GitHub refused the request (rate limit or token permissions)")
	case resp.StatusCode != http.StatusOK:
		return release{}, fmt.Errorf("check for updates: GitHub returned %s", resp.Status)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return release{}, fmt.Errorf("check for updates: %w", err)
	}
	rel := release{Tag: body.TagName, Notes: body.Body, URL: body.HTMLURL, Published: body.PublishedAt, assets: map[string]asset{}}
	for _, a := range body.Assets {
		rel.assets[a.Name] = asset{url: a.URL, size: a.Size}
	}
	if !parseVersion(rel.Tag).ok {
		return release{}, fmt.Errorf("latest release tag %q is not a version", rel.Tag)
	}
	return rel, nil
}

// fetch writes an asset to w, refusing anything larger than max bytes.
func (g github) fetch(ctx context.Context, a asset, max int64, w io.Writer) error {
	if a.size > max {
		return fmt.Errorf("release file is too large (%d bytes)", a.size)
	}
	resp, err := g.request(ctx, a.url, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: GitHub returned %s", resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	switch {
	case err != nil:
		return fmt.Errorf("download: %w", err)
	case n > max:
		return errors.New("release file is too large")
	}
	return nil
}

func (g github) get(ctx context.Context, a asset, max int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := g.fetch(ctx, a, max, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
