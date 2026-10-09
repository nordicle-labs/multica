// Package githubapp provides the server-only GitHub App credential broker.
package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const responseLimit = 1 << 20

type Permissions map[string]string

type Credential struct {
	Repository string `json:"repository"`
	Token      string `json:"token"`
}

type Broker struct {
	AppID      string
	PrivateKey string
	APIBase    string
	Client     *http.Client
	Now        func() time.Time
}

func PermissionsForRole(role string) (Permissions, error) {
	switch role {
	case "owner", "admin":
		return Permissions{"contents": "write", "metadata": "read", "pull_requests": "write"}, nil
	case "member":
		return Permissions{"contents": "read", "metadata": "read", "pull_requests": "read"}, nil
	default:
		return nil, fmt.Errorf("github App access denied for workspace role %q", role)
	}
}

func ParseRepository(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return "", errors.New("repository is not hosted on github.com")
	}
	path := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("invalid GitHub repository URL")
	}
	return parts[0] + "/" + parts[1], nil
}

func (b Broker) Mint(ctx context.Context, installationID int64, repository string, permissions Permissions) (Credential, error) {
	if installationID <= 0 || len(permissions) == 0 {
		return Credential{}, errors.New("github App installation and permissions are required")
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[1] == "" {
		return Credential{}, errors.New("invalid GitHub repository")
	}
	appJWT, err := b.SignJWT()
	if err != nil {
		return Credential{}, err
	}
	body, err := json.Marshal(map[string]any{"repositories": []string{parts[1]}, "permissions": permissions})
	if err != nil {
		return Credential{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.APIBase, "/")+fmt.Sprintf("/app/installations/%d/access_tokens", installationID), bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client().Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("create installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
		return Credential{}, fmt.Errorf("create installation token: github status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&out); err != nil {
		return Credential{}, errors.New("create installation token: malformed response")
	}
	if out.Token == "" {
		return Credential{}, errors.New("create installation token: empty token")
	}
	return Credential{Repository: repository, Token: out.Token}, nil
}

func (b Broker) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(b.APIBase, "/")+"/installation/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := b.client().Do(req)
	if err != nil {
		return fmt.Errorf("revoke installation token: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("revoke installation token: github status %d", resp.StatusCode)
	}
	return nil
}

func (b Broker) SignJWT() (string, error) {
	if strings.TrimSpace(b.AppID) == "" || strings.TrimSpace(b.PrivateKey) == "" {
		return "", errors.New("github App credentials unavailable")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(b.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("parse GitHub App private key: %w", err)
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strings.TrimSpace(b.AppID),
	})
	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed, nil
}

func (b Broker) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}
