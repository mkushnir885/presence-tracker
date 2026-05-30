package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cli/browser"
)

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	TokenType    string    `json:"token_type"`
}

// Valid reports whether the token is usable, treating it as expired 30s early
// so a request never goes out with a token about to lapse.
func (t *Token) Valid() bool {
	return t != nil && t.AccessToken != "" && time.Now().Before(t.Expiry.Add(-30*time.Second))
}

type Config struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Scopes       []string
	RedirectPort int
	TokenFile    string
}

// EnsureToken returns a usable token: from the on-disk cache if still valid,
// by refreshing it, or — as a last resort — by running the interactive browser
// PKCE flow.
func EnsureToken(ctx context.Context, cfg Config) (*Token, error) {
	tok, err := loadToken(cfg.TokenFile)
	if err == nil && tok.Valid() {
		return tok, nil
	}

	if err == nil && tok.RefreshToken != "" {
		refreshed, rerr := refreshToken(ctx, cfg, tok.RefreshToken)
		if rerr == nil {
			if serr := saveToken(cfg.TokenFile, refreshed); serr != nil {
				slog.Warn("oauth: could not persist refreshed token", "err", serr)
			}
			return refreshed, nil
		}
		slog.Warn("oauth: token refresh failed, re-authorising", "err", rerr)
	}

	tok, err = runPKCEFlow(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if serr := saveToken(cfg.TokenFile, tok); serr != nil {
		slog.Warn("oauth: could not persist token", "err", serr)
	}
	return tok, nil
}

func AuthorizedClient(ctx context.Context, cfg Config) (*http.Client, error) {
	tok, err := EnsureToken(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &bearerTransport{cfg: cfg, tok: tok},
	}, nil
}

// bearerTransport injects the Bearer token on every request and refreshes it
// in-band when stale, so callers get a self-renewing http.Client.
type bearerTransport struct {
	cfg Config
	tok *Token
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.tok.Valid() {
		refreshed, err := refreshToken(req.Context(), t.cfg, t.tok.RefreshToken)
		if err != nil {
			slog.Warn("oauth: background refresh failed", "err", err)
		} else {
			t.tok = refreshed
			if serr := saveToken(t.cfg.TokenFile, refreshed); serr != nil {
				slog.Warn("oauth: could not persist refreshed token", "err", serr)
			}
		}
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.tok.AccessToken)
	return http.DefaultTransport.RoundTrip(clone)
}

// runPKCEFlow runs the interactive Authorization Code + PKCE flow: spin up a
// localhost callback server, open the browser to the consent page, wait for
// the redirect carrying the code, then exchange it for a token.
func runPKCEFlow(ctx context.Context, cfg Config) (*Token, error) {
	verifier, challenge, err := pkceChallenge()
	if err != nil {
		return nil, fmt.Errorf("oauth: pkce: %w", err)
	}

	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("oauth: state: %w", err)
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", cfg.RedirectPort)

	params := url.Values{
		"client_id":             {cfg.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(cfg.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
	}
	authURL := cfg.AuthURL + "?" + params.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", cfg.RedirectPort))
	if err != nil {
		return nil, fmt.Errorf("oauth: listen on port %d: %w", cfg.RedirectPort, err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("state") != state {
				http.Error(w, "state mismatch", http.StatusBadRequest)
				errCh <- fmt.Errorf("oauth: state mismatch")
				return
			}
			if e := q.Get("error"); e != "" {
				http.Error(w, e, http.StatusBadRequest)
				errCh <- fmt.Errorf("oauth: provider error: %s", e)
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				errCh <- fmt.Errorf("oauth: missing code in callback")
				return
			}
			_, _ = fmt.Fprint(w, "<html><body><p>Authorisation successful. You can close this tab.</p></body></html>")
			codeCh <- code
		}),
	}

	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			errCh <- fmt.Errorf("oauth: callback server: %w", serr)
		}
	}()
	defer func() { //nolint:contextcheck // shuts down the temporary callback server with a fresh context after the flow ends
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("oauth: opening browser for authorisation", "url", authURL)
	if berr := browser.OpenURL(authURL); berr != nil {
		fmt.Printf("Open this URL in your browser to authorise:\n\n  %s\n\n", authURL)
	}

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return exchangeCode(ctx, cfg, code, verifier, redirectURI)
}

func exchangeCode(ctx context.Context, cfg Config, code, verifier, redirectURI string) (*Token, error) {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {cfg.ClientID},
		"code_verifier": {verifier},
	}
	if cfg.ClientSecret != "" {
		body.Set("client_secret", cfg.ClientSecret)
	}
	return postToken(ctx, cfg.TokenURL, body)
}

func refreshToken(ctx context.Context, cfg Config, refreshTok string) (*Token, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"client_id":     {cfg.ClientID},
	}
	if cfg.ClientSecret != "" {
		body.Set("client_secret", cfg.ClientSecret)
	}
	tok, err := postToken(ctx, cfg.TokenURL, body)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshTok
	}
	return tok, nil
}

func postToken(ctx context.Context, tokenURL string, body url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("oauth: parse token response: %w", err)
	}
	if raw.Error != "" {
		return nil, fmt.Errorf("oauth: token error %q: %s", raw.Error, raw.ErrorDesc)
	}
	expiry := time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	return &Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Expiry:       expiry,
		TokenType:    raw.TokenType,
	}, nil
}

func loadToken(path string) (*Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("oauth: parse token file: %w", err)
	}
	return &tok, nil
}

func saveToken(path string, tok *Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // path is the internal token-cache location, not user input
		return err
	}
	data, err := json.Marshal(tok) //nolint:gosec // the token is deliberately persisted to the local cache
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600) //nolint:gosec // path is the internal token-cache location, not user input
}

func pkceChallenge() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
