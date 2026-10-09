package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubCredentialSessionKeepsTokenOutOfEnvironmentAndDisk(t *testing.T) {
	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revoked = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	const token = "ghs_test_secret_token"
	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: token}})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{Agent: &AgentData{}, GitHubCredentials: []GitHubCredential{{Repository: "owner/repo", Token: token}}}
	session.apply(&task)
	if task.GitHubCredentials != nil {
		t.Fatal("raw GitHub token remained on the task after credential-helper setup")
	}
	for key, value := range task.Agent.CustomEnv {
		if strings.Contains(key, token) || strings.Contains(value, token) || key == "GITHUB_TOKEN" {
			t.Fatalf("token leaked through environment %q", key)
		}
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n	helper = !f() { echo username=personal; echo password=personal-token; }; f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(cmd.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
	for key, value := range task.Agent.CustomEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=owner/repo.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password="+token) {
		t.Fatalf("Git credential helper did not override the inherited personal helper: %s", out)
	}

	session.close(context.Background())
	if !revoked {
		t.Fatal("installation token was not revoked")
	}
}

func TestGitHubCredentialSessionRevokesTokenWhenHelperSetupFails(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })
	t.Setenv("PATH", t.TempDir())

	if _, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "ghs_must_revoke"}}); err == nil {
		t.Fatal("credential session succeeded without git")
	}
	if len(methods) != 1 || methods[0] != http.MethodDelete {
		t.Fatalf("revocation methods = %v, want [DELETE]", methods)
	}
}
