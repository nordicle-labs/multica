package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	BootstrapOwner   = "nordicle-labs"
	TemplateVersion  = "v1"
	DefaultBranch    = "main"
	maxResponseBytes = 1 << 20
)

var (
	//go:embed all:template
	templateFS    embed.FS
	validRepoName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$`)
)

var RequiredChecks = []string{"build", "lint", "test", "smoke"}

type BootstrapResult struct {
	State               string `json:"state"`
	RepositoryURL       string `json:"repository_url"`
	Branch              string `json:"branch"`
	HeadSHA             string `json:"head_sha"`
	TreeSHA             string `json:"tree_sha"`
	TemplateVersion     string `json:"template_version"`
	TemplateFingerprint string `json:"template_fingerprint"`
	RepositoryID        int64  `json:"-"`
}

type Bootstrapper struct{ Broker Broker }

// Check performs the complete remote preflight without mutation. A present
// repository is returned only after template and protection readback succeeds.
func (b Bootstrapper) Check(ctx context.Context, installationID int64, name, visibility, product string) (bool, BootstrapResult, error) {
	name, err := NormalizeRepositoryName(name)
	if err != nil {
		return false, BootstrapResult{}, err
	}
	if err := ValidateVisibility(visibility); err != nil {
		return false, BootstrapResult{}, err
	}
	fingerprint, err := TemplateFingerprint()
	if err != nil {
		return false, BootstrapResult{}, err
	}
	result := BootstrapResult{State: "unchanged", RepositoryURL: "https://github.com/" + BootstrapOwner + "/" + name, Branch: DefaultBranch, TemplateVersion: TemplateVersion, TemplateFingerprint: fingerprint}
	// A repository may not exist yet, so preflight needs an installation-wide
	// read-only token. It is revoked before repository-scoped verification.
	token, err := b.Broker.MintInstallation(ctx, installationID, Permissions{"metadata": "read"})
	if err != nil {
		return false, result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), token) }()
	exists, err := b.repositoryExists(ctx, token, name)
	if err != nil || !exists {
		return exists, result, err
	}
	if err := b.Broker.Revoke(ctx, token); err != nil {
		return true, result, err
	}
	token = ""
	credential, err := b.Broker.Mint(ctx, installationID, BootstrapOwner+"/"+name, Permissions{"administration": "read", "contents": "read", "metadata": "read"})
	if err != nil {
		return true, result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), credential.Token) }()
	if err := b.verify(ctx, credential.Token, name, visibility, product, fingerprint, &result); err != nil {
		return true, result, fmt.Errorf("existing repository is divergent: %w", err)
	}
	return true, result, nil
}

func NormalizeRepositoryName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !validRepoName.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".git") {
		return "", errors.New("name must be 1-100 lowercase letters, digits, dots, underscores, or hyphens")
	}
	return name, nil
}

func ValidateVisibility(value string) error {
	switch value {
	case "private", "internal", "public":
		return nil
	default:
		return errors.New("visibility must be private, internal, or public")
	}
}

func TemplateFingerprint() (string, error) {
	files, err := templateFiles()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, name := range sortedKeys(files) {
		_, _ = io.WriteString(h, name+"\x00")
		_, _ = h.Write(files[name])
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (b Bootstrapper) Bootstrap(ctx context.Context, installationID int64, name, visibility, product string) (result BootstrapResult, err error) {
	name, err = NormalizeRepositoryName(name)
	if err != nil {
		return result, err
	}
	if err = ValidateVisibility(visibility); err != nil {
		return result, err
	}
	product = strings.TrimSpace(product)
	if product == "" {
		return result, errors.New("product is required")
	}
	fingerprint, err := TemplateFingerprint()
	if err != nil {
		return result, err
	}
	result = BootstrapResult{RepositoryURL: "https://github.com/" + BootstrapOwner + "/" + name, Branch: DefaultBranch, TemplateVersion: TemplateVersion, TemplateFingerprint: fingerprint}

	// Repository creation is the only operation that cannot use a repository-
	// scoped token. Revoke this installation-wide token after attesting the ID.
	token, err := b.Broker.MintInstallation(ctx, installationID, Permissions{"administration": "write", "metadata": "read"})
	if err != nil {
		return result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), token) }()

	exists, err := b.repositoryExists(ctx, token, name)
	if err != nil {
		return result, err
	}
	if exists {
		if err := b.Broker.Revoke(ctx, token); err != nil {
			return result, err
		}
		token = ""
		credential, mintErr := b.Broker.Mint(ctx, installationID, BootstrapOwner+"/"+name, Permissions{"administration": "read", "contents": "read", "metadata": "read"})
		if mintErr != nil {
			return result, mintErr
		}
		defer func() { _ = b.Broker.Revoke(context.Background(), credential.Token) }()
		if err := b.verify(ctx, credential.Token, name, visibility, product, fingerprint, &result); err != nil {
			return result, fmt.Errorf("existing repository is divergent: %w", err)
		}
		result.State = "unchanged"
		return result, nil
	}

	created := false
	defer func() {
		if err == nil || !created {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if rollbackErr := b.Delete(rollbackCtx, installationID, name, result.RepositoryID); rollbackErr != nil {
			err = fmt.Errorf("%w; rollback incomplete; reconcile repository %s/%s", err, BootstrapOwner, name)
		}
	}()
	var creationAmbiguous bool
	result.RepositoryID, creationAmbiguous, err = b.createRepository(ctx, token, name, visibility)
	if err != nil {
		if creationAmbiguous {
			readbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err = b.reconcileAmbiguousCreate(readbackCtx, token, name, err)
		}
		return result, err
	}
	created = true
	if err = b.attestRepository(ctx, token, name, result.RepositoryID); err != nil {
		return result, err
	}
	if err = b.Broker.Revoke(ctx, token); err != nil {
		return result, err
	}
	token = ""
	credential, err := b.Broker.Mint(ctx, installationID, BootstrapOwner+"/"+name, Permissions{"administration": "write", "contents": "write", "metadata": "read"})
	if err != nil {
		return result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), credential.Token) }()
	if err = b.writeTemplate(ctx, credential.Token, name, product, fingerprint); err != nil {
		return result, err
	}
	if err = b.protect(ctx, credential.Token, name); err != nil {
		return result, err
	}
	if err = b.verify(ctx, credential.Token, name, visibility, product, fingerprint, &result); err != nil {
		return result, err
	}
	result.State = "created"
	return result, nil
}

func (b Bootstrapper) Delete(ctx context.Context, installationID int64, name string, expectedID int64) error {
	if expectedID <= 0 {
		return errors.New("delete repository: immutable repository ID is required")
	}
	credential, err := b.Broker.Mint(ctx, installationID, BootstrapOwner+"/"+name, Permissions{"administration": "write", "metadata": "read"})
	if err != nil {
		return err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), credential.Token) }()
	if err := b.attestRepository(ctx, credential.Token, name, expectedID); err != nil {
		return fmt.Errorf("delete repository identity check: %w", err)
	}
	status, _, err := b.request(ctx, credential.Token, http.MethodDelete, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || (status != http.StatusNoContent && status != http.StatusNotFound) {
		return fmt.Errorf("delete repository: github status %d: %w", status, err)
	}
	status, _, err = b.request(ctx, credential.Token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || status != http.StatusNotFound {
		return fmt.Errorf("verify repository deletion: github status %d: %w", status, err)
	}
	return nil
}

func (b Bootstrapper) repositoryExists(ctx context.Context, token, name string) (bool, error) {
	status, _, err := b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("repository preflight: github status %d", status)
	}
}

func (b Bootstrapper) createRepository(ctx context.Context, token, name, visibility string) (int64, bool, error) {
	status, raw, err := b.request(ctx, token, http.MethodPost, "/orgs/"+BootstrapOwner+"/repos", map[string]any{"name": name, "visibility": visibility, "private": visibility == "private", "auto_init": false})
	if err != nil || status != http.StatusCreated {
		return 0, err != nil || status == http.StatusCreated, fmt.Errorf("create repository: github status %d: %w", status, err)
	}
	var repo struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(raw, &repo) != nil || repo.ID <= 0 {
		return 0, true, errors.New("create repository: immutable repository ID missing")
	}
	return repo.ID, true, nil
}

func (b Bootstrapper) reconcileAmbiguousCreate(ctx context.Context, token, name string, createErr error) error {
	status, raw, err := b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err == nil && status == http.StatusNotFound {
		return fmt.Errorf("%w; repository creation readback confirmed absent", createErr)
	}
	if err == nil && status == http.StatusOK {
		var repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		}
		if json.Unmarshal(raw, &repo) == nil && repo.ID > 0 && repo.FullName == BootstrapOwner+"/"+name {
			return fmt.Errorf("%w; creation outcome is ambiguous; reconcile repository %s/%s (immutable ID %d)", createErr, BootstrapOwner, name, repo.ID)
		}
	}
	return fmt.Errorf("%w; creation outcome readback failed; reconcile repository %s/%s", createErr, BootstrapOwner, name)
}

func (b Bootstrapper) attestRepository(ctx context.Context, token, name string, expectedID int64) error {
	status, raw, err := b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || status != http.StatusOK {
		return errors.New("repository identity readback failed")
	}
	var repo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if json.Unmarshal(raw, &repo) != nil || repo.ID != expectedID || repo.FullName != BootstrapOwner+"/"+name {
		return errors.New("repository identity mismatch")
	}
	return nil
}

func (b Bootstrapper) writeTemplate(ctx context.Context, token, name, product, fingerprint string) error {
	files, err := canonicalTemplateFiles(product, fingerprint)
	if err != nil {
		return err
	}
	for _, path := range sortedKeys(files) {
		body := map[string]any{"message": "chore: initialize canonical template", "content": base64.StdEncoding.EncodeToString(files[path]), "branch": DefaultBranch}
		status, _, requestErr := b.request(ctx, token, http.MethodPut, "/repos/"+BootstrapOwner+"/"+name+"/contents/"+path, body)
		if requestErr != nil || (status != http.StatusCreated && status != http.StatusOK) {
			return fmt.Errorf("write template file %s: github status %d: %w", path, status, requestErr)
		}
	}
	return nil
}

func (b Bootstrapper) protect(ctx context.Context, token, name string) error {
	body := map[string]any{"required_status_checks": map[string]any{"strict": true, "contexts": RequiredChecks}, "enforce_admins": true, "required_pull_request_reviews": map[string]any{"required_approving_review_count": 1}, "restrictions": nil, "allow_force_pushes": false, "allow_deletions": false}
	status, _, err := b.request(ctx, token, http.MethodPut, "/repos/"+BootstrapOwner+"/"+name+"/branches/"+DefaultBranch+"/protection", body)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("protect main branch: github status %d: %w", status, err)
	}
	return nil
}

func (b Bootstrapper) verify(ctx context.Context, token, name, visibility, product, fingerprint string, result *BootstrapResult) error {
	status, raw, err := b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("repository readback failed")
	}
	var repo struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		Visibility    string `json:"visibility"`
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(raw, &repo) != nil || repo.ID <= 0 || repo.FullName != BootstrapOwner+"/"+name || repo.Visibility != visibility || repo.DefaultBranch != DefaultBranch {
		return errors.New("repository metadata mismatch")
	}
	result.RepositoryID = repo.ID
	expectedFiles, err := canonicalTemplateFiles(product, fingerprint)
	if err != nil {
		return err
	}
	for _, path := range sortedKeys(expectedFiles) {
		status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/contents/"+path, nil)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("template file %s readback failed", path)
		}
		var content struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &content) != nil {
			return fmt.Errorf("malformed template file %s response", path)
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
		if decodeErr != nil || !bytes.Equal(decoded, expectedFiles[path]) {
			return fmt.Errorf("template file %s mismatch", path)
		}
	}
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/branches/"+DefaultBranch+"/protection", nil)
	if err != nil || status != http.StatusOK {
		return errors.New("branch protection readback failed")
	}
	var protection struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
			Strict   *bool    `json:"strict"`
		} `json:"required_status_checks"`
		EnforceAdmins *struct {
			Enabled *bool `json:"enabled"`
		} `json:"enforce_admins"`
		RequiredPullRequestReviews *struct {
			RequiredApprovingReviewCount *int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
		Restrictions     json.RawMessage `json:"restrictions"`
		AllowForcePushes *struct {
			Enabled *bool `json:"enabled"`
		} `json:"allow_force_pushes"`
		AllowDeletions *struct {
			Enabled *bool `json:"enabled"`
		} `json:"allow_deletions"`
	}
	if json.Unmarshal(raw, &protection) != nil ||
		protection.RequiredStatusChecks == nil || protection.RequiredStatusChecks.Strict == nil || !*protection.RequiredStatusChecks.Strict ||
		strings.Join(protection.RequiredStatusChecks.Contexts, ",") != strings.Join(RequiredChecks, ",") ||
		protection.EnforceAdmins == nil || protection.EnforceAdmins.Enabled == nil || !*protection.EnforceAdmins.Enabled ||
		protection.RequiredPullRequestReviews == nil || protection.RequiredPullRequestReviews.RequiredApprovingReviewCount == nil || *protection.RequiredPullRequestReviews.RequiredApprovingReviewCount != 1 ||
		string(protection.Restrictions) != "null" ||
		protection.AllowForcePushes == nil || protection.AllowForcePushes.Enabled == nil || *protection.AllowForcePushes.Enabled ||
		protection.AllowDeletions == nil || protection.AllowDeletions.Enabled == nil || *protection.AllowDeletions.Enabled {
		return errors.New("branch protection mismatch")
	}
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/git/ref/heads/"+DefaultBranch, nil)
	if err != nil || status != http.StatusOK {
		return errors.New("branch readback failed")
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal(raw, &ref) != nil || ref.Object.SHA == "" {
		return errors.New("branch SHA missing")
	}
	result.HeadSHA = ref.Object.SHA
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/git/commits/"+ref.Object.SHA, nil)
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &commit) != nil || commit.Tree.SHA == "" {
		return errors.New("tree SHA readback failed")
	}
	result.TreeSHA = commit.Tree.SHA
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/git/trees/"+commit.Tree.SHA+"?recursive=1", nil)
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	expectedTree := expectedTreeEntries(expectedFiles)
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &tree) != nil || tree.Truncated || len(tree.Tree) != len(expectedTree) {
		return errors.New("repository tree inventory mismatch")
	}
	seen := make(map[string]struct{}, len(tree.Tree))
	for _, entry := range tree.Tree {
		if expectedTree[entry.Path] != entry.Type {
			return errors.New("repository tree inventory mismatch")
		}
		seen[entry.Path] = struct{}{}
	}
	if len(seen) != len(expectedTree) {
		return errors.New("repository tree inventory mismatch")
	}
	return nil
}

func (b Bootstrapper) request(ctx context.Context, token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.Broker.APIBase, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.Broker.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(raw) > maxResponseBytes {
		return resp.StatusCode, nil, errors.New("github response exceeds 1 MiB")
	}
	return resp.StatusCode, raw, nil
}

func templateFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(templateFS, "template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path, "template/")
		if name == "go.mod.template" {
			name = "go.mod"
		}
		files[name] = data
		return nil
	})
	return files, err
}

func canonicalTemplateFiles(product, fingerprint string) (map[string][]byte, error) {
	files, err := templateFiles()
	if err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(struct {
		Version     string `json:"version"`
		Fingerprint string `json:"fingerprint"`
		Product     string `json:"product"`
	}{TemplateVersion, fingerprint, strings.TrimSpace(product)}, "", "  ")
	if err != nil {
		return nil, err
	}
	files[".multica-template.json"] = append(manifest, '\n')
	return files, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func expectedTreeEntries[V any](files map[string]V) map[string]string {
	entries := make(map[string]string, len(files))
	for path := range files {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			entries[strings.Join(parts[:i], "/")] = "tree"
		}
		entries[path] = "blob"
	}
	return entries
}
