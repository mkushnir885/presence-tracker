package challenger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"presence-tracker/src/internal/config"
)

func TestLLMClientComplete(t *testing.T) {
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "hello"}},
			},
		})
	}))
	defer srv.Close()

	c := NewLLMClient(config.AIBackendConfig{BaseURL: srv.URL, Model: "qwen"})
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Errorf("content = %q", got)
	}
	if gotReq.Model != "qwen" {
		t.Errorf("model = %q", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Role != "user" {
		t.Errorf("messages = %+v", gotReq.Messages)
	}
}

func TestLLMClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewLLMClient(config.AIBackendConfig{BaseURL: srv.URL, Model: "x"})
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected error on 5xx")
	}

	c2 := NewLLMClient(config.AIBackendConfig{})
	if _, err := c2.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected error on empty base_url")
	}
}

func TestLLMClientNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	c := NewLLMClient(config.AIBackendConfig{BaseURL: srv.URL, Model: "x"})
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected error when no choices")
	}
}
