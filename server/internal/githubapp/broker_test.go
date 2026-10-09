package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

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
		_, _ = w.Write([]byte(`{"token":"secret-installation-token"}`))
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
