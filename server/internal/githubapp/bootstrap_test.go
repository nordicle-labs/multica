package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type bootstrapGitHub struct {
	mu             sync.Mutex
	repository     bool
	visibility     string
	files          map[string]string
	protected      bool
	createCount    int
	deleteCount    int
	failProtection bool
}

func (g *bootstrapGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	if strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens") {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"test-installation-token"}`))
		return
	}
	if path == "/installation/token" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if path == "/orgs/nordicle-labs/repos" {
		g.repository = true
		g.createCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.visibility, _ = body["visibility"].(string)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if path == "/repos/nordicle-labs/widget" && r.Method == http.MethodDelete {
		g.repository = false
		g.deleteCount++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if path == "/repos/nordicle-labs/widget" {
		if !g.repository {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"full_name": "nordicle-labs/widget", "visibility": g.visibility, "default_branch": "main"})
		return
	}
	const contents = "/repos/nordicle-labs/widget/contents/"
	if strings.HasPrefix(path, contents) {
		file := strings.TrimPrefix(path, contents)
		if r.Method == http.MethodPut {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"required_status_checks": map[string]any{"contexts": RequiredChecks}})
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
