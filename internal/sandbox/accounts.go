package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/tattsun/collie/internal/model"
)

// Claude accounts: a user can keep several named ways of reaching Claude
// (a claude.ai subscription, AWS Bedrock, an API key, anything else that is
// a set of environment variables) and choose which one each project uses.
//
// Layout under users/<uid>/claude/:
//
//	accounts.json   names, kinds, the default account, per-project choices
//	<id>.env        the account's KEY=VALUE lines (0600)
//
// The chosen account's env file is passed to every agent / shell exec (see
// envFiles), so a change reaches the next process without recreating the
// container.

const (
	KindSubscription = "subscription"
	KindBedrock      = "bedrock"
	KindAPIKey       = "apikey"
	KindCustom       = "custom"
)

const (
	EnvOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvUseBedrock = "CLAUDE_CODE_USE_BEDROCK"
)

// Account is the public face of an account; its values live in the env file.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type accountsFile struct {
	Accounts []Account         `json:"accounts"`
	Default  string            `json:"default,omitempty"`
	Projects map[string]string `json:"projects,omitempty"` // project id → account id
}

// AccountSpec says which variables a kind takes. Secret ones are never sent
// back to the browser, and an empty value on update keeps what is stored.
type AccountSpec struct {
	Fixed   map[string]string // always written (e.g. CLAUDE_CODE_USE_BEDROCK=1)
	Allowed []string
	Secret  map[string]bool
}

var specs = map[string]AccountSpec{
	KindSubscription: {Allowed: []string{EnvOAuthToken}, Secret: map[string]bool{EnvOAuthToken: true}},
	KindAPIKey: {
		Allowed: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"},
		Secret:  map[string]bool{"ANTHROPIC_API_KEY": true},
	},
	KindBedrock: {
		Fixed: map[string]string{EnvUseBedrock: "1"},
		Allowed: []string{"AWS_REGION", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
			"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"},
		Secret: map[string]bool{"AWS_BEARER_TOKEN_BEDROCK": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true},
	},
	KindCustom: {},
}

// KindSpec returns the variable set of a kind.
func KindSpec(kind string) (AccountSpec, bool) {
	s, ok := specs[kind]
	return s, ok
}

// AccountView is an account as the browser sees it: values except secrets
// (only whether they are set), or the raw text for the custom kind.
type AccountView struct {
	Account
	Values  map[string]string `json:"values,omitempty"`
	Secrets map[string]bool   `json:"secrets,omitempty"`
	Raw     string            `json:"raw,omitempty"`
}

// AccountsView is everything the Claude accounts screen needs.
type AccountsView struct {
	Accounts []AccountView     `json:"accounts"`
	Default  string            `json:"default"`
	Projects map[string]string `json:"projects"`
}

var accountsMu sync.Mutex

func (m *Manager) claudeDir(uid string) string { return filepath.Join(m.UserDir(uid), "claude") }

func (m *Manager) accountsPath(uid string) string {
	return filepath.Join(m.claudeDir(uid), "accounts.json")
}

// AccountEnvPath is the env file of an account.
func (m *Manager) AccountEnvPath(uid, id string) string {
	return filepath.Join(m.claudeDir(uid), id+".env")
}

func (m *Manager) loadAccounts(uid string) (accountsFile, error) {
	var f accountsFile
	b, err := os.ReadFile(m.accountsPath(uid))
	if errors.Is(err, os.ErrNotExist) {
		return f, m.migrateLegacyToken(uid, &f)
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	return f, nil
}

func (m *Manager) saveAccounts(uid string, f accountsFile) error {
	if err := os.MkdirAll(m.claudeDir(uid), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.accountsPath(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.accountsPath(uid))
}

// migrateLegacyToken turns a CLAUDE_CODE_OAUTH_TOKEN saved in "My
// environment" (how tokens used to be stored) into a first account.
func (m *Manager) migrateLegacyToken(uid string, f *accountsFile) error {
	b, err := os.ReadFile(m.UserEnvPath(uid))
	if err != nil {
		return nil // nothing to migrate; leave the file absent
	}
	var keep []string
	token := ""
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), EnvOAuthToken+"="); ok {
			token = strings.TrimSpace(v)
			continue
		}
		keep = append(keep, l)
	}
	if token == "" {
		return nil
	}
	a := Account{ID: newAccountID(), Name: "Claude subscription", Kind: KindSubscription}
	if err := writeEnv(m.AccountEnvPath(uid, a.ID), map[string]string{EnvOAuthToken: token}, nil); err != nil {
		return err
	}
	f.Accounts = []Account{a}
	f.Default = a.ID
	if err := m.saveAccounts(uid, *f); err != nil {
		return err
	}
	return os.WriteFile(m.UserEnvPath(uid), []byte(strings.TrimLeft(strings.Join(keep, "\n"), "\n")), 0o600)
}

func newAccountID() string { return model.NewID() }

// ParseEnv reads KEY=VALUE lines (comments and blanks skipped).
func ParseEnv(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			out[strings.TrimSpace(k)] = v
		}
	}
	return out
}

func readEnv(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseEnv(string(b))
}

// writeEnv writes vals (sorted, so the file is stable) after the lines of
// preface, if any.
func writeEnv(path string, vals map[string]string, preface []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, l := range preface {
		sb.WriteString(l + "\n")
	}
	for _, k := range keys {
		sb.WriteString(k + "=" + vals[k] + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// Accounts lists the user's Claude accounts.
func (m *Manager) Accounts(uid string) (AccountsView, error) {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	f, err := m.loadAccounts(uid)
	if err != nil {
		return AccountsView{}, err
	}
	v := AccountsView{Accounts: []AccountView{}, Default: f.Default, Projects: f.Projects}
	if v.Projects == nil {
		v.Projects = map[string]string{}
	}
	for _, a := range f.Accounts {
		av := AccountView{Account: a}
		if a.Kind == KindCustom {
			b, _ := os.ReadFile(m.AccountEnvPath(uid, a.ID))
			av.Raw = string(b)
		} else {
			sp := specs[a.Kind]
			av.Values, av.Secrets = map[string]string{}, map[string]bool{}
			for k, val := range readEnv(m.AccountEnvPath(uid, a.ID)) {
				if sp.Secret[k] {
					av.Secrets[k] = val != ""
				} else if _, fixed := sp.Fixed[k]; !fixed {
					av.Values[k] = val
				}
			}
		}
		v.Accounts = append(v.Accounts, av)
	}
	return v, nil
}

// AccountInput creates (ID empty) or updates an account. For the built-in
// kinds Values maps variable names to values: an empty value clears a plain
// variable and keeps a secret one. Clear lists variables to drop outright.
// Custom accounts take Raw instead.
type AccountInput struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Kind   string            `json:"kind"`
	Values map[string]string `json:"values"`
	Clear  []string          `json:"clear"`
	Raw    string            `json:"raw"`
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SaveAccount stores an account and returns its id. The first account
// becomes the default.
func (m *Manager) SaveAccount(uid string, in AccountInput) (string, error) {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 60 {
		return "", errors.New("give the account a name (up to 60 characters)")
	}
	f, err := m.loadAccounts(uid)
	if err != nil {
		return "", err
	}
	idx := -1
	for i, a := range f.Accounts {
		if a.ID == in.ID && in.ID != "" {
			idx = i
		} else if strings.EqualFold(a.Name, name) {
			return "", errors.New("an account with that name already exists")
		}
	}
	if in.ID != "" && idx < 0 {
		return "", errors.New("account not found")
	}
	kind := in.Kind
	if idx >= 0 {
		kind = f.Accounts[idx].Kind // the kind of an existing account does not change
	}
	sp, ok := specs[kind]
	if !ok {
		return "", fmt.Errorf("unknown account kind %q", kind)
	}
	id := in.ID
	if idx < 0 {
		id = newAccountID()
	}
	path := m.AccountEnvPath(uid, id)
	if kind == KindCustom {
		if err := ValidateEnvText(in.Raw); err != nil {
			return "", err
		}
		if err := os.MkdirAll(m.claudeDir(uid), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(in.Raw), 0o600); err != nil {
			return "", err
		}
	} else {
		vals := readEnv(path)
		allowed := map[string]bool{}
		for _, k := range sp.Allowed {
			allowed[k] = true
		}
		for _, k := range in.Clear {
			if allowed[k] {
				delete(vals, k)
			}
		}
		for k, v := range in.Values {
			if !allowed[k] {
				return "", fmt.Errorf("%s is not a setting of this account type", k)
			}
			v = strings.TrimSpace(v)
			if strings.ContainsAny(v, "\r\n") {
				return "", fmt.Errorf("%s must be a single line", k)
			}
			switch {
			case v != "":
				vals[k] = v
			case !sp.Secret[k]:
				delete(vals, k)
			}
		}
		for k, v := range sp.Fixed {
			vals[k] = v
		}
		if idx < 0 && kind == KindSubscription && vals[EnvOAuthToken] == "" {
			return "", errors.New("the token is required")
		}
		if err := writeEnv(path, vals, nil); err != nil {
			return "", err
		}
	}
	a := Account{ID: id, Name: name, Kind: kind}
	if idx >= 0 {
		f.Accounts[idx] = a
	} else {
		f.Accounts = append(f.Accounts, a)
		if f.Default == "" {
			f.Default = id
		}
	}
	return id, m.saveAccounts(uid, f)
}

// SaveSubscriptionToken stores a `claude setup-token` token: into the
// account id if given, else into the user's first subscription account
// (creating one named name when there is none).
func (m *Manager) SaveSubscriptionToken(uid, id, name, token string) (string, error) {
	token = strings.Join(strings.Fields(token), "") // tokens copied from a narrow terminal may carry line breaks
	if token == "" {
		return "", errors.New("the token is empty")
	}
	if id == "" {
		v, err := m.Accounts(uid)
		if err != nil {
			return "", err
		}
		for _, a := range v.Accounts {
			if a.Kind == KindSubscription {
				id = a.ID
				name = a.Name
				break
			}
		}
	}
	if name == "" {
		name = "Claude subscription"
	}
	return m.SaveAccount(uid, AccountInput{ID: id, Name: name, Kind: KindSubscription, Values: map[string]string{EnvOAuthToken: token}})
}

// DeleteAccount removes an account; projects that used it fall back to the
// default (which moves to another account if it was the one deleted).
func (m *Manager) DeleteAccount(uid, id string) error {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	f, err := m.loadAccounts(uid)
	if err != nil {
		return err
	}
	var keep []Account
	for _, a := range f.Accounts {
		if a.ID != id {
			keep = append(keep, a)
		}
	}
	if len(keep) == len(f.Accounts) {
		return errors.New("account not found")
	}
	f.Accounts = keep
	if f.Default == id {
		f.Default = ""
		if len(keep) > 0 {
			f.Default = keep[0].ID
		}
	}
	for p, a := range f.Projects {
		if a == id {
			delete(f.Projects, p)
		}
	}
	_ = os.Remove(m.AccountEnvPath(uid, id))
	return m.saveAccounts(uid, f)
}

func hasAccount(f accountsFile, id string) bool {
	for _, a := range f.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// SetDefaultAccount picks the account used by projects without a choice.
func (m *Manager) SetDefaultAccount(uid, id string) error {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	f, err := m.loadAccounts(uid)
	if err != nil {
		return err
	}
	if !hasAccount(f, id) {
		return errors.New("account not found")
	}
	f.Default = id
	return m.saveAccounts(uid, f)
}

// SetProjectAccount picks the account the user's agents in a project use
// ("" goes back to the default).
func (m *Manager) SetProjectAccount(uid, projectID, id string) error {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	f, err := m.loadAccounts(uid)
	if err != nil {
		return err
	}
	if id != "" && !hasAccount(f, id) {
		return errors.New("account not found")
	}
	if f.Projects == nil {
		f.Projects = map[string]string{}
	}
	if id == "" {
		delete(f.Projects, projectID)
	} else {
		f.Projects[projectID] = id
	}
	return m.saveAccounts(uid, f)
}

// accountFile is the env file of the account the user's agents use in the
// project ("" when there is none).
func (m *Manager) accountFile(projectID, uid string) string {
	accountsMu.Lock()
	f, err := m.loadAccounts(uid)
	accountsMu.Unlock()
	if err != nil {
		return ""
	}
	id := f.Projects[projectID]
	if !hasAccount(f, id) {
		id = f.Default
	}
	if !hasAccount(f, id) {
		return ""
	}
	return m.AccountEnvPath(uid, id)
}

// ValidateEnvText checks KEY=VALUE lines.
func ValidateEnvText(content string) error {
	for i, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, _, ok := strings.Cut(l, "=")
		if !ok || !envKeyRe.MatchString(strings.TrimSpace(k)) {
			return fmt.Errorf("line %d: expected KEY=VALUE", i+1)
		}
	}
	return nil
}
