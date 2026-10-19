package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestPermissionsForRoleFailsClosed(t *testing.T) {
	for _, role := range []string{"", "guest", "developer"} {
		if _, err := PermissionsForRole(role); err == nil {
			t.Fatalf("role %q was allowed", role)
		}
	}
	write, _ := PermissionsForRole("admin")
	read, _ := PermissionsForRole("member")
	if write["contents"] != "write" || read["contents"] != "read" {
		t.Fatalf("unexpected permissions: admin=%v member=%v", write, read)
	}
}

func TestMintScopesTokenToOneRepositoryAndPermissions(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/42/access_tokens" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"secret-installation-token","repositories":[{"full_name":"owner/repo"}]}`))
	}))
	defer server.Close()

	permissions, _ := PermissionsForRole("member")
	broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	credential, err := broker.Mint(context.Background(), 42, "owner/repo", permissions)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Repository != "owner/repo" || credential.Token == "" {
		t.Fatalf("credential = %+v", credential)
	}
	if !reflect.DeepEqual(got["repositories"], []any{"repo"}) {
		t.Fatalf("repositories = %#v", got["repositories"])
	}
	gotPermissions := got["permissions"].(map[string]any)
	if gotPermissions["contents"] != "read" || gotPermissions["pull_requests"] != "read" {
		t.Fatalf("permissions = %#v", gotPermissions)
	}
}

func TestMintRejectsTokenScopedToDifferentOwner(t *testing.T) {
	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"wrong-scope","repositories":[{"full_name":"other/repo"}]}`))
	}))
	defer server.Close()
	permissions, _ := PermissionsForRole("member")
	broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client()}
	if _, err := broker.Mint(context.Background(), 42, "owner/repo", permissions); err == nil {
		t.Fatal("Mint accepted a token scoped to another owner")
	}
	if !revoked {
		t.Fatal("Mint did not revoke the mismatched token")
	}
}

func TestMintRevokesTokenFromMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name            string
		revokeStatus    int
		wantRevokeError bool
	}{
		{name: "revoked", revokeStatus: http.StatusNoContent},
		{name: "revoke failed", revokeStatus: http.StatusInternalServerError, wantRevokeError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := strings.Repeat("x", 40)
			deleteCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleteCount++
					w.WriteHeader(tc.revokeStatus)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":"2099-01-01T00:00:00Z","repositories":[{"full_name":"owner/repo"}]} trailing`, token)
			}))
			defer server.Close()

			broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
			permissions, _ := PermissionsForRole("member")
			_, err := broker.Mint(context.Background(), 42, "owner/repo", permissions)
			if err == nil || !strings.Contains(err.Error(), "malformed response") {
				t.Fatalf("Mint error = %v", err)
			}
			if deleteCount != 1 {
				t.Fatalf("DELETE count = %d, want 1", deleteCount)
			}
			if got := strings.Contains(err.Error(), "revoke installation token"); got != tc.wantRevokeError {
				t.Fatalf("revocation error present = %v, want %v: %v", got, tc.wantRevokeError, err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("Mint exposed the token in its error")
			}
		})
	}
}

func TestMintInstallationRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"must-not-be-accepted"}` + strings.Repeat(" ", responseLimit)))
	}))
	defer server.Close()

	broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client()}
	if _, err := broker.MintInstallation(context.Background(), 42, Permissions{"metadata": "read"}); err == nil {
		t.Fatal("MintInstallation accepted a response larger than the limit")
	}
}

func TestMintInstallationRevokesTokenFromMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name            string
		revokeStatus    int
		wantRevokeError bool
	}{
		{name: "revoked", revokeStatus: http.StatusNoContent},
		{name: "revoke failed", revokeStatus: http.StatusInternalServerError, wantRevokeError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := strings.Repeat("x", 40)
			deleteCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleteCount++
					w.WriteHeader(tc.revokeStatus)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":"2099-01-01T00:00:00Z"} trailing`, token)
			}))
			defer server.Close()

			broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
			_, err := broker.MintInstallation(context.Background(), 42, Permissions{"metadata": "read"})
			if err == nil || !strings.Contains(err.Error(), "malformed response") {
				t.Fatalf("MintInstallation error = %v", err)
			}
			if deleteCount != 1 {
				t.Fatalf("DELETE count = %d, want 1", deleteCount)
			}
			if got := strings.Contains(err.Error(), "revoke installation token"); got != tc.wantRevokeError {
				t.Fatalf("revocation error present = %v, want %v: %v", got, tc.wantRevokeError, err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("MintInstallation exposed the token in its error")
			}
		})
	}
}

func TestMintInstallationRejectsInvalidExpiration(t *testing.T) {
	for _, response := range []string{
		`{"token":"must-not-be-accepted"}`,
		`{"token":"must-not-be-accepted","expires_at":"not-a-time"}`,
		`{"token":"must-not-be-accepted","expires_at":"2000-01-01T00:00:00Z"}`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()

			broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
			if _, err := broker.MintInstallation(context.Background(), 42, Permissions{"metadata": "read"}); err == nil {
				t.Fatal("MintInstallation accepted an invalid expiration")
			}
		})
	}
}

func TestBrokerRefusesRedirectWithAppJWT(t *testing.T) {
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: source.URL, Client: source.Client()}
	if _, err := broker.MintInstallation(context.Background(), 42, Permissions{"metadata": "read"}); err == nil {
		t.Fatal("MintInstallation followed a redirect")
	}
	if redirectedAuthorization != "" {
		t.Fatal("App JWT was forwarded to the redirect target")
	}
}

func TestBrokerRefusesRedirectWithInstallationToken(t *testing.T) {
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	broker := Broker{APIBase: source.URL, Client: source.Client()}
	if err := broker.Revoke(context.Background(), "installation-secret"); err == nil {
		t.Fatal("Revoke followed a redirect")
	}
	if redirectedAuthorization != "" {
		t.Fatal("installation token was forwarded to the redirect target")
	}
}

func TestMintFailureDistinguishesRejectedAndUncertainRequests(t *testing.T) {
	permissions, _ := PermissionsForRole("member")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	broker := Broker{AppID: "7", PrivateKey: testPrivateKey(t), APIBase: server.URL, Client: server.Client()}
	_, err := broker.Mint(context.Background(), 42, "owner/repo", permissions)
	var failure *MintFailure
	if !errors.As(err, &failure) || !failure.NoCredential || !failure.Retryable {
		t.Fatalf("HTTP rejection failure = %#v, %v", failure, err)
	}

	broker.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("response lost")
	})}
	_, err = broker.Mint(context.Background(), 42, "owner/repo", permissions)
	failure = nil
	if !errors.As(err, &failure) || failure.NoCredential || !failure.Retryable {
		t.Fatalf("uncertain network failure = %#v, %v", failure, err)
	}
}

func TestParseRepository(t *testing.T) {
	for _, raw := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git"} {
		got, err := ParseRepository(raw)
		if err != nil || got != "owner/repo" {
			t.Fatalf("ParseRepository(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"https://example.com/owner/repo", "https://github.com/owner"} {
		if _, err := ParseRepository(raw); err == nil {
			t.Fatalf("ParseRepository(%q) succeeded", raw)
		}
	}
}
