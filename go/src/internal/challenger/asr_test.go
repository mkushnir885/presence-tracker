package challenger

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"presence-tracker/src/internal/config"
)

func TestASRClientTranscribe(t *testing.T) {
	tests := []struct {
		name      string
		respCode  int
		respBody  string
		wantText  string
		wantError bool
	}{
		{name: "ok", respCode: 200, respBody: `{"text":"hello world"}`, wantText: "hello world"},
		{name: "trims whitespace", respCode: 200, respBody: `{"text":"  hi  "}`, wantText: "hi"},
		{name: "server error", respCode: 503, respBody: `boom`, wantError: true},
		{name: "malformed json", respCode: 200, respBody: `not-json`, wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth, gotCT string
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				w.WriteHeader(tc.respCode)
				_, _ = io.WriteString(w, tc.respBody)
			}))
			defer srv.Close()

			c := NewASRClient(config.AIBackendConfig{
				BaseURL: srv.URL,
				APIKey:  "sk-test",
				Model:   "whisper",
			}, "uk-UA")

			text, err := c.Transcribe(context.Background(), bytes.NewBufferString("audio-bytes"), "audio/webm")
			if (err != nil) != tc.wantError {
				t.Fatalf("err = %v, wantError = %v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if gotPath != "/audio/transcriptions" {
				t.Errorf("path = %q", gotPath)
			}
			if gotAuth != "Bearer sk-test" {
				t.Errorf("auth = %q", gotAuth)
			}
			if !strings.HasPrefix(gotCT, "multipart/form-data") {
				t.Errorf("content-type = %q", gotCT)
			}
			if !bytes.Contains(gotBody, []byte("audio-bytes")) {
				t.Errorf("body did not include audio payload: %q", gotBody)
			}
			if !bytes.Contains(gotBody, []byte(`name="model"`)) {
				t.Errorf("body missing model field")
			}
			if !bytes.Contains(gotBody, []byte(`name="language"`)) ||
				!bytes.Contains(gotBody, []byte("uk")) ||
				bytes.Contains(gotBody, []byte("uk-UA")) {
				t.Errorf("body should carry language=uk (region stripped); got %q", gotBody)
			}
		})
	}
}

func TestASRClientOmitsLanguageWhenUnset(t *testing.T) {
	for _, sentinel := range []string{"", "auto", "AUTO", " auto "} {
		t.Run("sentinel="+sentinel, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.WriteHeader(200)
				_, _ = io.WriteString(w, `{"text":""}`)
			}))
			defer srv.Close()

			c := NewASRClient(config.AIBackendConfig{BaseURL: srv.URL, Model: "whisper"}, sentinel)
			if _, err := c.Transcribe(context.Background(), bytes.NewBufferString("x"), "audio/webm"); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(gotBody, []byte(`name="language"`)) {
				t.Errorf("language hint leaked for sentinel %q: %q", sentinel, gotBody)
			}
		})
	}
}

func TestASRClientNoBaseURL(t *testing.T) {
	c := NewASRClient(config.AIBackendConfig{}, "")
	if _, err := c.Transcribe(context.Background(), bytes.NewBufferString("x"), "audio/webm"); err == nil {
		t.Fatal("expected error when base_url is empty")
	}
}
