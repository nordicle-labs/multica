package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type tokenMint struct {
	repositories []string
	permissions  Permissions
}

type bootstrapGitHub struct {
	mu                  sync.Mutex
	repository          bool
	repositoryReads     int
	visibility          string
	files               map[string]string
	protected           bool
	createCount         int
	deleteCount         int
	failCreate          bool
	createThenFail      bool
	failTemplate        bool
	failProtection      bool
	failDelete          bool
	omitProtectionFlags bool
	malformedCreate     bool
	malformedRepoRead   int
	disconnectCreate    bool
	revokeCount         int
	failRevokeAt        int
	tokenMints          []tokenMint
}

func TestRepositoryIdentity(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/nordicle-labs/example",
		"https://github.com/nordicle-labs/example/",
		"https://github.com/nordicle-labs/example.git",
		"git@github.com:nordicle-labs/example.git",
	} {
		got, err := RepositoryIdentity(raw)
		if err != nil || got != "nordicle-labs/example" {
			t.Errorf("RepositoryIdentity(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"https://gitlab.com/nordicle-labs/example",
		"https://github.com/nordicle-labs",
		"https://github.com/nordicle-labs/example/extra",
		"https://github.com/nordicle-labs/example?ref=main",
		"git@evil.example:nordicle-labs/example.git",
		"not a repository",
	} {
		if _, err := RepositoryIdentity(raw); err == nil {
			t.Errorf("RepositoryIdentity(%q) unexpectedly succeeded", raw)
		}
	}
}

func (g *bootstrapGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	if strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens") {
		var body struct {
			Repositories []string    `json:"repositories"`
			Permissions  Permissions `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.tokenMints = append(g.tokenMints, tokenMint{repositories: body.Repositories, permissions: body.Permissions})
		w.WriteHeader(http.StatusCreated)
		if len(body.Repositories) == 1 {
			_, _ = w.Write([]byte(`{"token":"test-installation-token","expires_at":"2099-01-01T00:00:00Z","repositories":[{"full_name":"nordicle-labs/widget"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"token":"test-installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		}
		return
	}
	if path == "/installation/token" {
		g.revokeCount++
		if g.revokeCount == g.failRevokeAt {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if path == "/orgs/nordicle-labs/repos" {
		if g.failCreate {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		g.repository = true
		g.createCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.visibility, _ = body["visibility"].(string)
		if g.createThenFail {
			http.Error(w, "injected after creation", http.StatusInternalServerError)
			return
		}
		if g.disconnectCreate {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				http.Error(w, "hijack", http.StatusInternalServerError)
				return
			}
			_ = connection.Close()
			return
		}
		w.WriteHeader(http.StatusCreated)
		if g.malformedCreate {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":123}`))
		return
	}
	if path == "/repos/nordicle-labs/widget" && r.Method == http.MethodDelete {
		if g.failDelete {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		g.repository = false
		g.deleteCount++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if path == "/repos/nordicle-labs/widget" {
		g.repositoryReads++
		if !g.repository {
			http.NotFound(w, r)
			return
		}
		if g.malformedRepoRead == g.repositoryReads {
			_, _ = w.Write([]byte(`{`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "full_name": "nordicle-labs/widget", "visibility": g.visibility, "default_branch": "main"})
		return
	}
	const contents = "/repos/nordicle-labs/widget/contents/"
	if strings.HasPrefix(path, contents) {
		file := strings.TrimPrefix(path, contents)
		if r.Method == http.MethodPut {
			if g.failTemplate {
				http.Error(w, "injected", http.StatusInternalServerError)
				return
			}
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.files[file] = body.Content
			w.WriteHeader(http.StatusCreated)
			return
		}
		content, ok := g.files[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": content})
		return
	}
	if strings.HasSuffix(path, "/branches/main/protection") {
		if r.Method == http.MethodPut {
			if g.failProtection {
				http.Error(w, "injected", http.StatusInternalServerError)
				return
			}
			g.protected = true
		}
		if !g.protected {
			http.NotFound(w, r)
			return
		}
		protection := map[string]any{
			"required_status_checks":        map[string]any{"strict": true, "contexts": RequiredChecks},
			"enforce_admins":                map[string]bool{"enabled": true},
			"required_pull_request_reviews": map[string]int{"required_approving_review_count": 1},
			"restrictions":                  nil,
		}
		if !g.omitProtectionFlags {
			protection["allow_force_pushes"] = map[string]bool{"enabled": false}
			protection["allow_deletions"] = map[string]bool{"enabled": false}
		}
		_ = json.NewEncoder(w).Encode(protection)
		return
	}
	if strings.HasSuffix(path, "/git/ref/heads/main") {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "head-sha"}})
		return
	}
	if strings.HasSuffix(path, "/git/commits/head-sha") {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "tree-sha"}})
		return
	}
	if strings.HasSuffix(path, "/git/trees/tree-sha") {
		tree := expectedTreeEntries(g.files)
		entries := make([]map[string]string, 0, len(tree))
		for path, entryType := range tree {
			entries = append(entries, map[string]string{"path": path, "type": entryType})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"truncated": false, "tree": entries})
		return
	}
	http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, path), http.StatusNotFound)
}

func testBootstrapper(t *testing.T, state *bootstrapGitHub) Bootstrapper {
	t.Helper()
	if state.files == nil {
		state.files = map[string]string{}
	}
	server := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(server.Close)
	return Bootstrapper{Broker: Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client()}}
}

func TestBootstrapCreatesCanonicalRepositoryAndSecondPassIsUnchanged(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}}
	bootstrapper := testBootstrapper(t, state)
	first, err := bootstrapper.Bootstrap(context.Background(), 42, "Widget", "private", "Widget Product")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "created" || first.HeadSHA != "head-sha" || first.TreeSHA != "tree-sha" {
		t.Fatalf("first = %+v", first)
	}
	second, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product")
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "unchanged" || state.createCount != 1 {
		t.Fatalf("second=%+v creates=%d", second, state.createCount)
	}
	wantMints := []tokenMint{
		{permissions: Permissions{"administration": "write", "metadata": "read"}},
		{repositories: []string{"widget"}, permissions: Permissions{"administration": "write", "contents": "write", "metadata": "read", "workflows": "write"}},
		{permissions: Permissions{"administration": "write", "metadata": "read"}},
		{repositories: []string{"widget"}, permissions: Permissions{"administration": "read", "contents": "read", "metadata": "read"}},
	}
	if !reflect.DeepEqual(state.tokenMints, wantMints) {
		t.Fatalf("token mints = %#v, want %#v", state.tokenMints, wantMints)
	}
	for _, path := range []string{"README.md", "AGENTS.md", ".github/CODEOWNERS", ".github/workflows/ci.yml", ".multica/environment.json", ".multica-template.json", "Makefile"} {
		if _, ok := state.files[path]; !ok {
			t.Errorf("missing template file %s", path)
		}
	}
	manifest, _ := base64.StdEncoding.DecodeString(state.files[".multica-template.json"])
	if !strings.Contains(string(manifest), first.TemplateFingerprint) {
		t.Fatal("manifest does not carry template fingerprint")
	}
}

func TestBootstrapRejectsExistingRepositoryWithChangedTemplateFile(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}}
	bootstrapper := testBootstrapper(t, state)
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err != nil {
		t.Fatal(err)
	}
	state.files["README.md"] = base64.StdEncoding.EncodeToString([]byte("changed\n"))
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err == nil {
		t.Fatal("changed template file was accepted as unchanged")
	}
}

func TestBootstrapRejectsUnexpectedRemoteFile(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}}
	bootstrapper := testBootstrapper(t, state)
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err != nil {
		t.Fatal(err)
	}
	state.files["UNEXPECTED.txt"] = base64.StdEncoding.EncodeToString([]byte("unexpected\n"))
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err == nil || !strings.Contains(err.Error(), "tree inventory") {
		t.Fatalf("unexpected remote file error = %v", err)
	}
}

func TestBootstrapRejectsIncompleteProtectionReadback(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}}
	bootstrapper := testBootstrapper(t, state)
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err != nil {
		t.Fatal(err)
	}
	state.omitProtectionFlags = true
	if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product"); err == nil || !strings.Contains(err.Error(), "protection mismatch") {
		t.Fatalf("incomplete protection error = %v", err)
	}
}

func TestBootstrapReportsReconciliationAfterMalformedCreatedResponse(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, malformedCreate: true}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product")
	if err == nil || !strings.Contains(err.Error(), "reconcile repository nordicle-labs/widget") {
		t.Fatalf("malformed create error = %v", err)
	}
	if !state.repository || state.deleteCount != 0 {
		t.Fatalf("repository=%v deletes=%d", state.repository, state.deleteCount)
	}
}

func TestBootstrapReadsBackLostCreateResponseAndRequiresReconciliation(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, disconnectCreate: true}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product")
	if err == nil || !strings.Contains(err.Error(), "reconcile repository nordicle-labs/widget") {
		t.Fatalf("lost create response error = %v", err)
	}
	if !state.repository || state.deleteCount != 0 || state.repositoryReads < 2 {
		t.Fatalf("repository=%v deletes=%d reads=%d", state.repository, state.deleteCount, state.repositoryReads)
	}
}

func TestBootstrapReadsBackCreatedRepositoryAfterServerError(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, createThenFail: true}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product")
	if err == nil || !strings.Contains(err.Error(), "reconcile repository nordicle-labs/widget") {
		t.Fatalf("ambiguous create error = %v", err)
	}
	if !state.repository || state.deleteCount != 0 || state.repositoryReads < 2 {
		t.Fatalf("repository=%v deletes=%d reads=%d", state.repository, state.deleteCount, state.repositoryReads)
	}
}

func TestBootstrapFailsWhenSuccessfulBootstrapTokenRevocationFails(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, failRevokeAt: 2}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget Product")
	if err == nil || !strings.Contains(err.Error(), "revoke installation token") || !strings.Contains(err.Error(), "nordicle-labs/widget") {
		t.Fatalf("revocation error = %v", err)
	}
	if state.repository || state.deleteCount != 1 || state.revokeCount != 3 {
		t.Fatalf("repository=%v deletes=%d revocations=%d", state.repository, state.deleteCount, state.revokeCount)
	}
}

func TestBootstrapRollsBackOnlyRepositoryCreatedByCall(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, failProtection: true}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "internal", "Widget")
	if err == nil {
		t.Fatal("expected injected protection failure")
	}
	if state.repository || state.deleteCount != 1 {
		t.Fatalf("repository=%v deletes=%d", state.repository, state.deleteCount)
	}
}

func TestBootstrapFailureBoundariesRollbackCreatedRepository(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *bootstrapGitHub
	}{
		{name: "template write", state: &bootstrapGitHub{failTemplate: true}},
		{name: "branch protection", state: &bootstrapGitHub{failProtection: true}},
		{name: "malformed final repository readback", state: &bootstrapGitHub{malformedRepoRead: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootstrapper := testBootstrapper(t, tc.state)
			if _, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget"); err == nil {
				t.Fatal("expected injected failure")
			}
			if tc.state.repository || tc.state.deleteCount != 1 {
				t.Fatalf("repository=%v deletes=%d", tc.state.repository, tc.state.deleteCount)
			}
		})
	}
}

func TestBootstrapReportsIncompleteRemoteRollback(t *testing.T) {
	state := &bootstrapGitHub{files: map[string]string{}, failProtection: true, failDelete: true}
	bootstrapper := testBootstrapper(t, state)
	_, err := bootstrapper.Bootstrap(context.Background(), 42, "widget", "private", "Widget")
	if err == nil || !strings.Contains(err.Error(), "rollback incomplete; reconcile repository nordicle-labs/widget") {
		t.Fatalf("rollback error = %v", err)
	}
	if !state.repository || state.deleteCount != 0 {
		t.Fatalf("repository=%v deletes=%d", state.repository, state.deleteCount)
	}
}

func TestDeleteRefusesRepositoryIdentityMismatch(t *testing.T) {
	state := &bootstrapGitHub{repository: true, visibility: "private", files: map[string]string{}}
	bootstrapper := testBootstrapper(t, state)
	if err := bootstrapper.Delete(context.Background(), 42, "widget", 999); err == nil {
		t.Fatal("Delete accepted a different immutable repository ID")
	}
	if !state.repository || state.deleteCount != 0 {
		t.Fatalf("repository=%v deletes=%d", state.repository, state.deleteCount)
	}
}

func TestCanonicalTemplateIsDeterministicAndContainsNoSecrets(t *testing.T) {
	first, err := TemplateFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := TemplateFingerprint()
	if first != second || len(first) != 64 {
		t.Fatalf("fingerprints %q %q", first, second)
	}
	files, err := templateFiles()
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, value := range files {
		joined += string(value)
	}
	for _, forbidden := range []string{"ghp_", "github_pat_", "BEGIN PRIVATE KEY", "test-installation-token"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("template contains secret marker %q", forbidden)
		}
	}
	for _, command := range []string{"make build", "make test", "make lint", "make smoke", "make rollback"} {
		if !strings.Contains(joined, command) {
			t.Errorf("template missing %q", command)
		}
	}
}

func TestCanonicalTemplateCommandsExecute(t *testing.T) {
	fingerprint, err := TemplateFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	files, err := canonicalTemplateFiles("Widget", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, data := range files {
		fullPath := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"build", "test", "lint", "smoke", "rollback"} {
		cmd := exec.Command("make", target)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make %s: %v\n%s", target, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("rollback left artifacts: %v", err)
	}
}

func TestNormalizeRepositoryNameFailsBeforeHTTP(t *testing.T) {
	for _, input := range []string{"", "../repo", "repo.git", "UPPER SPACE", strings.Repeat("a", 101)} {
		if _, err := NormalizeRepositoryName(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
	if got, err := NormalizeRepositoryName(" Widget "); err != nil || got != "widget" {
		t.Fatalf("got %q, %v", got, err)
	}
}
