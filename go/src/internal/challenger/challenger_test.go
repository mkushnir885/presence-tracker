package challenger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
)

func autoGenCfg(t *testing.T, ag config.AutoGenerationConfig) *config.Config {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"challenges": map[string]any{"auto_generation": ag},
	})
	if err != nil {
		t.Fatalf("marshal seed config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load seed config: %v", err)
	}
	return cfg
}

type fakeDispatcher struct {
	mu            sync.Mutex
	calls         []challenges.Bank
	autoSubmitted []bool
	failErr       error
}

func (f *fakeDispatcher) RunPollBank(_ context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, bank)
	f.autoSubmitted = append(f.autoSubmitted, autoSubmitted)
	if f.failErr != nil {
		return challenges.PollResult{}, f.failErr
	}
	return challenges.PollResult{ScheduledCount: len(bank.Questions)}, nil
}

type fakeSink struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeSink) RecordGeneratorFailed(_ context.Context, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, reason)
}

func newFakeBackends(t *testing.T, asrText, llmYAML string) (asrURL, llmURL string) {
	t.Helper()
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": asrText})
	}))
	t.Cleanup(asr.Close)

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": llmYAML}},
			},
		})
	}))
	t.Cleanup(llm.Close)

	return asr.URL, llm.URL
}

func TestGenerateDispatchesOnAutoSubmit(t *testing.T) {
	asrURL, llmURL := newFakeBackends(t, strings.Repeat("alpha beta gamma delta epsilon ", 20), validYAML)

	disp := &fakeDispatcher{}
	sink := &fakeSink{}
	svc := New(autoGenCfg(t, config.AutoGenerationConfig{
		Enabled:             true,
		AutoSubmit:          true,
		PollIntervalSeconds: 30,
		MinWordsPerQuestion: 30,
		MaxQuestionsPerPoll: 5,
		ASR:                 config.AIBackendConfig{BaseURL: asrURL, Model: "whisper"},
		LLM:                 config.AIBackendConfig{BaseURL: llmURL, Model: "qwen"},
	}), disp, sink)

	res, err := svc.Generate(context.Background(), strings.NewReader("audio"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusGenerated {
		t.Fatalf("status = %v, reason = %v", res.Status, res.Reason)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatch calls = %d", len(disp.calls))
	}
	if !disp.autoSubmitted[0] {
		t.Errorf("auto_submitted = false, want true")
	}
	if len(sink.events) != 0 {
		t.Errorf("unexpected failure events: %v", sink.events)
	}
}

func TestGenerateSilenceSkips(t *testing.T) {
	asrURL, llmURL := newFakeBackends(t, "hi ok bye", "")
	svc := New(autoGenCfg(t, config.AutoGenerationConfig{
		Enabled:             true,
		AutoSubmit:          true,
		MinWordsPerQuestion: 30,
		MaxQuestionsPerPoll: 5,
		ASR:                 config.AIBackendConfig{BaseURL: asrURL},
		LLM:                 config.AIBackendConfig{BaseURL: llmURL},
	}), &fakeDispatcher{}, &fakeSink{})
	res, err := svc.Generate(context.Background(), strings.NewReader("x"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Errorf("status = %v", res.Status)
	}
}

func TestGenerateBelowThresholdHolds(t *testing.T) {
	asrURL, llmURL := newFakeBackends(t, "five six seven eight nine ten", "")
	svc := New(autoGenCfg(t, config.AutoGenerationConfig{
		Enabled:             true,
		AutoSubmit:          true,
		MinWordsPerQuestion: 30,
		MaxQuestionsPerPoll: 5,
		ASR:                 config.AIBackendConfig{BaseURL: asrURL},
		LLM:                 config.AIBackendConfig{BaseURL: llmURL},
	}), &fakeDispatcher{}, &fakeSink{})
	res, err := svc.Generate(context.Background(), strings.NewReader("x"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped || res.Reason != "below_threshold" {
		t.Errorf("status=%v reason=%v", res.Status, res.Reason)
	}
	if res.Words != 6 || res.Needed != 30 {
		t.Errorf("words=%d needed=%d", res.Words, res.Needed)
	}
}

func TestGenerateLLMFailureKeepsAccumulator(t *testing.T) {
	asrText := strings.Repeat("alpha beta gamma delta epsilon ", 20)
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": asrText})
	}))
	t.Cleanup(asr.Close)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	}))
	t.Cleanup(llm.Close)

	sink := &fakeSink{}
	svc := New(autoGenCfg(t, config.AutoGenerationConfig{
		Enabled:             true,
		AutoSubmit:          true,
		MinWordsPerQuestion: 30,
		MaxQuestionsPerPoll: 5,
		ASR:                 config.AIBackendConfig{BaseURL: asr.URL},
		LLM:                 config.AIBackendConfig{BaseURL: llm.URL},
	}), &fakeDispatcher{}, sink)

	res, err := svc.Generate(context.Background(), strings.NewReader("x"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.Reason != "llm_error" {
		t.Errorf("status=%v reason=%v", res.Status, res.Reason)
	}
	if svc.words == 0 {
		t.Error("accumulator was cleared after LLM failure; expected retention")
	}
	if len(sink.events) != 1 {
		t.Errorf("expected one failure event, got %d", len(sink.events))
	}
}

func TestGenerateReviewDirOnNoAutoSubmit(t *testing.T) {
	asrURL, llmURL := newFakeBackends(t, strings.Repeat("alpha beta gamma delta epsilon ", 20), validYAML)
	dir := t.TempDir()
	svc := New(autoGenCfg(t, config.AutoGenerationConfig{
		Enabled:             true,
		AutoSubmit:          false,
		MinWordsPerQuestion: 30,
		MaxQuestionsPerPoll: 5,
		ReviewDir:           dir,
		BankBasename:        "generated",
		ASR:                 config.AIBackendConfig{BaseURL: asrURL},
		LLM:                 config.AIBackendConfig{BaseURL: llmURL},
	}), &fakeDispatcher{}, &fakeSink{})

	res, err := svc.Generate(context.Background(), strings.NewReader("x"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusGenerated {
		t.Fatalf("status=%v reason=%v", res.Status, res.Reason)
	}
	if _, err := os.Stat(svc.review.FilePath()); err != nil {
		t.Fatalf("expected pending bank on disk: %v", err)
	}
}
