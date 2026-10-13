package main

import (
	"strings"
	"testing"
)

func TestRepoPublishUsesHostCredentialEnvironmentOnly(t *testing.T) {
	t.Setenv("MULTICA_GITHUB_APP_TOKEN", "")
	if repoPublishCmd.Flags().Lookup("token") != nil {
		t.Fatal("repo publish exposes the GitHub App token in argv")
	}
	err := runRepoPublish(repoPublishCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "MULTICA_GITHUB_APP_TOKEN is required") {
		t.Fatalf("missing credential error = %v", err)
	}
}
