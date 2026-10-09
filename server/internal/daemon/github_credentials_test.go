package daemon

import (
	"context"

	"net/http"
	"net/http/httptest"
	"os/exec"
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

	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0="+session.helper,
		"GIT_CONFIG_KEY_1=credential.useHttpPath",
		"GIT_CONFIG_VALUE_1=true",
	)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=owner/repo.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password="+token) {
		t.Fatal("Git credential helper did not return the task token")
	}

	session.close(context.Background())
	if !revoked {
		t.Fatal("installation token was not revoked")
	}
}
