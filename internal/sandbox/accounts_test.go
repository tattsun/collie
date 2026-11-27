package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyTokenMigratesToAccount(t *testing.T) {
	m := newTestManager(t)
	_ = os.MkdirAll(m.UserDir("u1"), 0o700)
	_ = os.WriteFile(m.UserEnvPath("u1"), []byte("GH_TOKEN=gh\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abc\n"), 0o600)
	v, err := m.Accounts("u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Accounts) != 1 || v.Accounts[0].Kind != KindSubscription || v.Default != v.Accounts[0].ID {
		t.Fatalf("migration: %+v", v)
	}
	if !v.Accounts[0].Secrets[EnvOAuthToken] {
		t.Fatal("token should be reported as set")
	}
	env, _ := os.ReadFile(m.UserEnvPath("u1"))
	if strings.Contains(string(env), "OAUTH") || !strings.Contains(string(env), "GH_TOKEN=gh") {
		t.Fatalf("user env after migration: %q", env)
	}
	// Idempotent.
	if v2, _ := m.Accounts("u1"); len(v2.Accounts) != 1 {
		t.Fatalf("second load: %+v", v2)
	}
}

func TestBedrockAccountAndProjectChoice(t *testing.T) {
	m := newTestManager(t)
	sub, err := m.SaveSubscriptionToken("u1", "", "", "sk-ant-oat01-aaa\nbbb")
	if err != nil {
		t.Fatal(err)
	}
	bed, err := m.SaveAccount("u1", AccountInput{Name: "Work Bedrock", Kind: KindBedrock, Values: map[string]string{
		"AWS_REGION": "us-east-1", "AWS_BEARER_TOKEN_BEDROCK": "tok", "ANTHROPIC_MODEL": "",
	}})
	if err != nil {
		t.Fatal(err)
	}
	env := readEnv(m.AccountEnvPath("u1", bed))
	if env[EnvUseBedrock] != "1" || env["AWS_REGION"] != "us-east-1" || env["AWS_BEARER_TOKEN_BEDROCK"] != "tok" {
		t.Fatalf("bedrock env: %v", env)
	}
	if got := readEnv(m.AccountEnvPath("u1", sub))[EnvOAuthToken]; got != "sk-ant-oat01-aaabbb" {
		t.Fatalf("token whitespace should be dropped, got %q", got)
	}

	// Default is the first account; a project can pick another.
	if got := m.accountFile("p1", "u1"); got != m.AccountEnvPath("u1", sub) {
		t.Fatalf("default: %s", got)
	}
	if err := m.SetProjectAccount("u1", "p1", bed); err != nil {
		t.Fatal(err)
	}
	if got := m.accountFile("p1", "u1"); got != m.AccountEnvPath("u1", bed) {
		t.Fatalf("project choice: %s", got)
	}
	if got := m.accountFile("p2", "u1"); got != m.AccountEnvPath("u1", sub) {
		t.Fatalf("other project: %s", got)
	}
	files := m.envFiles("p1", "u1")
	if len(files) == 0 || files[len(files)-1] != m.AccountEnvPath("u1", bed) {
		t.Fatalf("envFiles: %v", files)
	}

	// Editing: an empty secret keeps the stored one, clearing drops it.
	if _, err := m.SaveAccount("u1", AccountInput{ID: bed, Name: "Work Bedrock", Values: map[string]string{"AWS_REGION": "ap-northeast-1", "AWS_BEARER_TOKEN_BEDROCK": ""}}); err != nil {
		t.Fatal(err)
	}
	env = readEnv(m.AccountEnvPath("u1", bed))
	if env["AWS_REGION"] != "ap-northeast-1" || env["AWS_BEARER_TOKEN_BEDROCK"] != "tok" {
		t.Fatalf("after edit: %v", env)
	}
	if _, err := m.SaveAccount("u1", AccountInput{ID: bed, Name: "Work Bedrock", Clear: []string{"AWS_BEARER_TOKEN_BEDROCK"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readEnv(m.AccountEnvPath("u1", bed))["AWS_BEARER_TOKEN_BEDROCK"]; ok {
		t.Fatal("secret should be cleared")
	}
	if _, err := m.SaveAccount("u1", AccountInput{ID: bed, Name: "x", Values: map[string]string{"PATH": "/evil"}}); err == nil {
		t.Fatal("variables outside the kind must be rejected")
	}
	if _, err := m.SaveAccount("u1", AccountInput{Name: "work bedrock", Kind: KindBedrock}); err == nil {
		t.Fatal("duplicate names must be rejected")
	}

	// Deleting the chosen account falls back to the default.
	if err := m.DeleteAccount("u1", bed); err != nil {
		t.Fatal(err)
	}
	if got := m.accountFile("p1", "u1"); got != m.AccountEnvPath("u1", sub) {
		t.Fatalf("after delete: %s", got)
	}
	if _, err := os.Stat(filepath.Join(m.claudeDir("u1"), bed+".env")); !os.IsNotExist(err) {
		t.Fatal("env file should be gone")
	}
}

func TestCustomAccountRaw(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.SaveAccount("u1", AccountInput{Name: "gw", Kind: KindCustom, Raw: "not env"}); err == nil {
		t.Fatal("invalid raw env must be rejected")
	}
	id, err := m.SaveAccount("u1", AccountInput{Name: "gw", Kind: KindCustom, Raw: "ANTHROPIC_BASE_URL=https://gw\nANTHROPIC_AUTH_TOKEN=x\n"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := m.Accounts("u1")
	if v.Accounts[0].ID != id || !strings.Contains(v.Accounts[0].Raw, "ANTHROPIC_AUTH_TOKEN=x") {
		t.Fatalf("%+v", v)
	}
}
