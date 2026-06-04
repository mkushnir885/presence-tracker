package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Token.Valid ----

func TestTokenValid(t *testing.T) {
	future := time.Now().Add(time.Hour)
	soon := time.Now().Add(20 * time.Second) // within the 30s early-expiry window

	tests := []struct {
		name string
		tok  *Token
		want bool
	}{
		{"nil token", nil, false},
		{"empty access token", &Token{Expiry: future}, false},
		{"valid token", &Token{AccessToken: "tok", Expiry: future}, true},
		{"expires within 30s", &Token{AccessToken: "tok", Expiry: soon}, false},
		{"already expired", &Token{AccessToken: "tok", Expiry: time.Now().Add(-time.Second)}, false},
		{"exactly at boundary", &Token{AccessToken: "tok", Expiry: time.Now().Add(30 * time.Second)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.Valid(); got != tc.want {
				t.Errorf("Valid() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- pkceChallenge ----

func TestPKCEChallenge(t *testing.T) {
	verifier, challenge, err := pkceChallenge()
	if err != nil {
		t.Fatalf("pkceChallenge: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatal("expected non-empty verifier and challenge")
	}

	// challenge must equal base64url(SHA256(verifier)) — the S256 method.
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Errorf("challenge = %q, want %q", challenge, want)
	}

	// verifier must be URL-safe base64 (no padding, no +, no /).
	if strings.ContainsAny(verifier, "+/=") {
		t.Errorf("verifier contains non-URL-safe chars: %q", verifier)
	}
}

func TestPKCEChallengeUnique(t *testing.T) {
	v1, _, _ := pkceChallenge()
	v2, _, _ := pkceChallenge()
	if v1 == v2 {
		t.Error("two pkceChallenge calls produced the same verifier")
	}
}

// ---- randomState ----

func TestRandomState(t *testing.T) {
	s, err := randomState()
	if err != nil {
		t.Fatalf("randomState: %v", err)
	}
	if s == "" {
		t.Fatal("expected non-empty state")
	}
	if strings.ContainsAny(s, "+/=") {
		t.Errorf("state contains non-URL-safe chars: %q", s)
	}

	s2, _ := randomState()
	if s == s2 {
		t.Error("two randomState calls produced the same value")
	}
}

// ---- saveToken / loadToken ----

func TestSaveAndLoadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	tok := &Token{
		AccessToken:  "access123",
		RefreshToken: "refresh456",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}

	if err := saveToken(path, tok); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	loaded, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if loaded.AccessToken != tok.AccessToken {
		t.Errorf("AccessToken: got %q, want %q", loaded.AccessToken, tok.AccessToken)
	}
	if loaded.RefreshToken != tok.RefreshToken {
		t.Errorf("RefreshToken: got %q, want %q", loaded.RefreshToken, tok.RefreshToken)
	}
	if !loaded.Expiry.Equal(tok.Expiry) {
		t.Errorf("Expiry: got %v, want %v", loaded.Expiry, tok.Expiry)
	}
}

func TestSaveTokenCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "token.json")
	if err := saveToken(path, &Token{AccessToken: "x"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("token file not created: %v", err)
	}
}

func TestSaveTokenMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, &Token{AccessToken: "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode: got %o, want 0600", mode)
	}
}

func TestLoadTokenMissing(t *testing.T) {
	_, err := loadToken(filepath.Join(t.TempDir(), "no-such-file.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadTokenInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadToken(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ---- postToken ----

func tokenServer(t *testing.T, resp any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestPostToken(t *testing.T) {
	expiry := time.Now().Add(3600 * time.Second)
	srv := tokenServer(t, map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"expires_in":    3600,
		"token_type":    "Bearer",
	})
	defer srv.Close()

	tok, err := postToken(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("postToken: %v", err)
	}
	if tok.AccessToken != "new-access" {
		t.Errorf("AccessToken: %q", tok.AccessToken)
	}
	if tok.RefreshToken != "new-refresh" {
		t.Errorf("RefreshToken: %q", tok.RefreshToken)
	}
	if tok.Expiry.Before(expiry.Add(-5 * time.Second)) {
		t.Errorf("Expiry too early: %v", tok.Expiry)
	}
}

func TestPostTokenErrorResponse(t *testing.T) {
	// Providers often return HTTP 200 even for error responses.
	srv := tokenServer(t, map[string]any{
		"error":             "invalid_client",
		"error_description": "bad credentials",
	})
	defer srv.Close()

	_, err := postToken(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("expected error from error response")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("error message: %q", err.Error())
	}
}

func TestPostTokenSendsContentType(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "expires_in": 3600})
	}))
	defer srv.Close()

	_, _ = postToken(context.Background(), srv.URL, nil)
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type: %q", gotContentType)
	}
}

// ---- refreshToken ----

func TestRefreshTokenKeepsOldRefreshWhenServerOmitsIt(t *testing.T) {
	srv := tokenServer(t, map[string]any{
		"access_token": "new-access",
		"expires_in":   3600,
		// no refresh_token field
	})
	defer srv.Close()

	cfg := Config{TokenURL: srv.URL}
	tok, err := refreshToken(context.Background(), cfg, "original-refresh")
	if err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if tok.RefreshToken != "original-refresh" {
		t.Errorf("RefreshToken: got %q, want original-refresh", tok.RefreshToken)
	}
}

func TestRefreshTokenUsesNewRefreshWhenProvided(t *testing.T) {
	srv := tokenServer(t, map[string]any{
		"access_token":  "new-access",
		"refresh_token": "rotated-refresh",
		"expires_in":    3600,
	})
	defer srv.Close()

	cfg := Config{TokenURL: srv.URL}
	tok, err := refreshToken(context.Background(), cfg, "old-refresh")
	if err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if tok.RefreshToken != "rotated-refresh" {
		t.Errorf("RefreshToken: got %q, want rotated-refresh", tok.RefreshToken)
	}
}

func TestRefreshTokenIncludesClientSecret(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body = r.PostForm.Encode()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "expires_in": 3600})
	}))
	defer srv.Close()

	cfg := Config{TokenURL: srv.URL, ClientID: "id1", ClientSecret: "secret1"}
	_, err := refreshToken(context.Background(), cfg, "reftok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "client_secret=secret1") {
		t.Errorf("client_secret not in request body: %q", body)
	}
	if !strings.Contains(body, "grant_type=refresh_token") {
		t.Errorf("grant_type not in request body: %q", body)
	}
}

// ---- bearerTransport ----

func TestBearerTransportInjectsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer resource.Close()

	tok := &Token{AccessToken: "my-token", Expiry: time.Now().Add(time.Hour)}
	tr := &bearerTransport{cfg: Config{}, tok: tok}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, resource.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if gotAuth != "Bearer my-token" {
		t.Errorf("Authorization: got %q, want %q", gotAuth, "Bearer my-token")
	}
}

func TestBearerTransportRefreshesExpiredToken(t *testing.T) {
	// Token server returns a fresh token.
	tokenSrv := tokenServer(t, map[string]any{
		"access_token":  "refreshed-token",
		"refresh_token": "new-refresh",
		"expires_in":    3600,
	})
	defer tokenSrv.Close()

	// Resource server captures which token it received.
	var gotAuth string
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer resource.Close()

	// Construct an already-expired token so bearerTransport will refresh it.
	expired := &Token{
		AccessToken:  "old-token",
		RefreshToken: "old-refresh",
		Expiry:       time.Now().Add(-time.Minute),
	}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	tr := &bearerTransport{
		cfg: Config{TokenURL: tokenSrv.URL, TokenFile: tokenFile},
		tok: expired,
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, resource.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if gotAuth != "Bearer refreshed-token" {
		t.Errorf("Authorization after refresh: got %q, want %q", gotAuth, "Bearer refreshed-token")
	}
}

// ---- EnsureToken ----

func TestEnsureTokenReturnsCachedValidToken(t *testing.T) {
	tok := &Token{
		AccessToken:  "cached",
		RefreshToken: "ref",
		Expiry:       time.Now().Add(time.Hour),
	}
	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, tok); err != nil {
		t.Fatal(err)
	}

	// TokenURL left empty — any HTTP call would fail, proving none is made.
	cfg := Config{TokenFile: path}
	got, err := EnsureToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got.AccessToken != "cached" {
		t.Errorf("AccessToken: got %q, want cached", got.AccessToken)
	}
}

func TestEnsureTokenRefreshesExpiredCachedToken(t *testing.T) {
	srv := tokenServer(t, map[string]any{
		"access_token":  "refreshed",
		"refresh_token": "new-ref",
		"expires_in":    3600,
	})
	defer srv.Close()

	expired := &Token{
		AccessToken:  "old",
		RefreshToken: "old-ref",
		Expiry:       time.Now().Add(-time.Minute),
	}
	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, expired); err != nil {
		t.Fatal(err)
	}

	cfg := Config{TokenURL: srv.URL, TokenFile: path}
	got, err := EnsureToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got.AccessToken != "refreshed" {
		t.Errorf("AccessToken: got %q, want refreshed", got.AccessToken)
	}
}
