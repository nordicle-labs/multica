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
}

type Bootstrapper struct{ Broker Broker }

// Check performs the complete remote preflight without mutation. A present
// repository is returned only after template and protection readback succeeds.
func (b Bootstrapper) Check(ctx context.Context, installationID int64, name, visibility string) (bool, BootstrapResult, error) {
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
	token, err := b.Broker.MintInstallation(ctx, installationID, Permissions{"administration": "write", "contents": "write", "metadata": "read"})
	if err != nil {
		return false, result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), token) }()
	exists, err := b.repositoryExists(ctx, token, name)
	if err != nil || !exists {
		return exists, result, err
	}
	if err := b.verify(ctx, token, name, visibility, fingerprint, &result); err != nil {
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

	token, err := b.Broker.MintInstallation(ctx, installationID, Permissions{"administration": "write", "contents": "write", "metadata": "read"})
	if err != nil {
		return result, err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), token) }()

	exists, err := b.repositoryExists(ctx, token, name)
	if err != nil {
		return result, err
	}
	if exists {
		if err := b.verify(ctx, token, name, visibility, fingerprint, &result); err != nil {
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
		if rollbackErr := b.Delete(ctx, installationID, name); rollbackErr != nil {
			err = fmt.Errorf("%w; rollback incomplete; reconcile repository %s/%s", err, BootstrapOwner, name)
		}
	}()
	if err = b.createRepository(ctx, token, name, visibility); err != nil {
		return result, err
	}
	created = true
	if err = b.writeTemplate(ctx, token, name, product, fingerprint); err != nil {
		return result, err
	}
	if err = b.protect(ctx, token, name); err != nil {
		return result, err
	}
	if err = b.verify(ctx, token, name, visibility, fingerprint, &result); err != nil {
		return result, err
	}
	result.State = "created"
	return result, nil
}

func (b Bootstrapper) Delete(ctx context.Context, installationID int64, name string) error {
	token, err := b.Broker.MintInstallation(ctx, installationID, Permissions{"administration": "write", "metadata": "read"})
	if err != nil {
		return err
	}
	defer func() { _ = b.Broker.Revoke(context.Background(), token) }()
	status, _, err := b.request(ctx, token, http.MethodDelete, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || (status != http.StatusNoContent && status != http.StatusNotFound) {
		return fmt.Errorf("delete repository: github status %d: %w", status, err)
	}
	status, _, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
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

func (b Bootstrapper) createRepository(ctx context.Context, token, name, visibility string) error {
	status, _, err := b.request(ctx, token, http.MethodPost, "/orgs/"+BootstrapOwner+"/repos", map[string]any{"name": name, "visibility": visibility, "private": visibility == "private", "auto_init": false})
	if err != nil || status != http.StatusCreated {
		return fmt.Errorf("create repository: github status %d: %w", status, err)
	}
	return nil
}

func (b Bootstrapper) writeTemplate(ctx context.Context, token, name, product, fingerprint string) error {
	files, err := templateFiles()
	if err != nil {
		return err
	}
	files[".multica-template.json"] = []byte(fmt.Sprintf("{\n  \"version\": %q,\n  \"fingerprint\": %q,\n  \"product\": %q\n}\n", TemplateVersion, fingerprint, product))
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

func (b Bootstrapper) verify(ctx context.Context, token, name, visibility, fingerprint string, result *BootstrapResult) error {
	status, raw, err := b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name, nil)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("repository readback failed")
	}
	var repo struct {
		FullName      string `json:"full_name"`
		Visibility    string `json:"visibility"`
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(raw, &repo) != nil || repo.FullName != BootstrapOwner+"/"+name || repo.Visibility != visibility || repo.DefaultBranch != DefaultBranch {
		return errors.New("repository metadata mismatch")
	}
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/contents/.multica-template.json", nil)
	if err != nil || status != http.StatusOK {
		return errors.New("template manifest readback failed")
	}
	var content struct{ Content string }
	if json.Unmarshal(raw, &content) != nil {
		return errors.New("malformed template manifest response")
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if decodeErr != nil || !bytes.Contains(decoded, []byte(`"fingerprint": "`+fingerprint+`"`)) {
		return errors.New("template fingerprint mismatch")
	}
	status, raw, err = b.request(ctx, token, http.MethodGet, "/repos/"+BootstrapOwner+"/"+name+"/branches/"+DefaultBranch+"/protection", nil)
	if err != nil || status != http.StatusOK {
		return errors.New("branch protection readback failed")
	}
	var protection struct {
		RequiredStatusChecks struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
	}
	if json.Unmarshal(raw, &protection) != nil || strings.Join(protection.RequiredStatusChecks.Contexts, ",") != strings.Join(RequiredChecks, ",") {
		return errors.New("required checks mismatch")
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
		files[strings.TrimPrefix(path, "template/")] = data
		return nil
	})
	return files, err
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
