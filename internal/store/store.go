// Package store persists projects, sessions and per-session timelines.
//
// Metadata lives in a single state.json that is rewritten (debounced,
// atomically) on change. Timelines are append-only JSONL files where the
// latest line for a given item id wins.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
)

type state struct {
	Projects []*model.Project `json:"projects"`
	Sessions []*model.Session `json:"sessions"`
}

type Store struct {
	dir string

	mu       sync.Mutex
	projects map[string]*model.Project
	sessions map[string]*model.Session
	timer    *time.Timer
	dirty    bool

	logMu sync.Mutex
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "uploads"), filepath.Join(dir, "worktrees")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, projects: map[string]*model.Project{}, sessions: map[string]*model.Session{}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		var st state
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, err
		}
		for _, p := range st.Projects {
			s.projects[p.ID] = p
		}
		for _, x := range st.Sessions {
			s.sessions[x.ID] = x
		}
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) scheduleSave() {
	s.dirty = true
	if s.timer == nil {
		s.timer = time.AfterFunc(250*time.Millisecond, func() { _ = s.Flush() })
	}
}

// Flush writes state.json if there are pending changes.
func (s *Store) Flush() error {
	s.mu.Lock()
	s.timer = nil
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	st := state{}
	for _, p := range s.projects {
		c := *p
		c.Status, c.Error = "", ""
		st.Projects = append(st.Projects, &c)
	}
	for _, x := range s.sessions {
		c := *x
		st.Sessions = append(st.Sessions, &c)
	}
	s.mu.Unlock()
	sort.Slice(st.Projects, func(i, j int) bool { return st.Projects[i].CreatedAt < st.Projects[j].CreatedAt })
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].CreatedAt < st.Sessions[j].CreatedAt })
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

// ---- projects ----

func (s *Store) Projects() []model.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (s *Store) Project(id string) (model.Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return model.Project{}, false
	}
	return *p, true
}

func (s *Store) PutProject(p model.Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects[p.ID] = &p
	s.scheduleSave()
}

func (s *Store) UpdateProject(id string, fn func(*model.Project)) (model.Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return model.Project{}, false
	}
	fn(p)
	s.scheduleSave()
	return *p, true
}

func (s *Store) DeleteProject(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projects, id)
	s.scheduleSave()
}

// ---- sessions ----

func (s *Store) Sessions() []model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Session, 0, len(s.sessions))
	for _, x := range s.sessions {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

func (s *Store) Session(id string) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.sessions[id]
	if !ok {
		return model.Session{}, false
	}
	return *x, true
}

func (s *Store) PutSession(x model.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[x.ID] = &x
	s.scheduleSave()
}

func (s *Store) UpdateSession(id string, fn func(*model.Session)) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.sessions[id]
	if !ok {
		return model.Session{}, false
	}
	fn(x)
	s.scheduleSave()
	return *x, true
}

func (s *Store) DeleteSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.scheduleSave()
	s.mu.Unlock()
	s.logMu.Lock()
	_ = os.Remove(s.logPath(id))
	s.logMu.Unlock()
}

// ---- timelines ----

func (s *Store) logPath(sid string) string {
	return filepath.Join(s.dir, "sessions", sid+".jsonl")
}

func (s *Store) AppendItem(sid string, it *model.Item) error {
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.logMu.Lock()
	defer s.logMu.Unlock()
	f, err := os.OpenFile(s.logPath(sid), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// LoadItems folds the timeline log: items keep the position of their first
// appearance and the content of their last.
func (s *Store) LoadItems(sid string) ([]*model.Item, int, error) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	f, err := os.Open(s.logPath(sid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var items []*model.Item
	lines := 0
	idx := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		lines++
		var it model.Item
		if json.Unmarshal(sc.Bytes(), &it) != nil || it.ID == "" {
			continue
		}
		if i, ok := idx[it.ID]; ok {
			items[i] = &it
		} else {
			idx[it.ID] = len(items)
			items = append(items, &it)
		}
	}
	return items, lines, sc.Err()
}

// CompactItems rewrites the log with one line per item.
func (s *Store) CompactItems(sid string, items []*model.Item) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	tmp := s.logPath(sid) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.logPath(sid))
}

func (s *Store) UploadsDir() string   { return filepath.Join(s.dir, "uploads") }
func (s *Store) WorktreesDir() string { return filepath.Join(s.dir, "worktrees") }
