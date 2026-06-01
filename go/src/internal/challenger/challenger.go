package challenger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
)

// silenceFloorWords drops ASR results shorter than this, the cheapest
// defence against Whisper hallucinating phrases ("Thanks for watching!")
// on near-silent input.
const silenceFloorWords = 5

type Status string

const (
	StatusGenerated Status = "generated"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
)

type Result struct {
	Status     Status `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Questions  int    `json:"questions,omitempty"`
	Words      int    `json:"words,omitempty"`
	Needed     int    `json:"needed,omitempty"`
	BankPath   string `json:"bank_path,omitempty"`
	AutoSubmit bool   `json:"auto_submit,omitempty"`
}

// Dispatcher hands a generated bank straight to the running poll pipeline
// (the auto_submit path); implemented by the session coordinator.
type Dispatcher interface {
	RunPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error)
}

type EventSink interface {
	RecordGeneratorFailed(ctx context.Context, reason string)
}

// Service re-reads the auto-generation config on every Generate so
// mid-session edits take effect on the next interval without restart.
type Service struct {
	cfg        *config.Config
	dispatcher Dispatcher
	sink       EventSink

	mu          sync.Mutex
	accumulator strings.Builder
	words       int

	asr    *ASRClient
	llm    *LLMClient
	prod   *Producer
	review *ReviewPath
	last   config.AutoGenerationConfig
	have   bool
}

func New(cfg *config.Config, dispatcher Dispatcher, sink EventSink) *Service {
	return &Service{
		cfg:        cfg,
		dispatcher: dispatcher,
		sink:       sink,
	}
}

func (s *Service) resetAccumulator() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulator.Reset()
	s.words = 0
}

func (s *Service) resolve() config.AutoGenerationConfig {
	ag := s.cfg.Get().Challenges.AutoGeneration
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.have &&
		s.last.ASR == ag.ASR &&
		s.last.LLM == ag.LLM &&
		s.last.Language == ag.Language &&
		stringSliceEqual(s.last.ExtraRules, ag.ExtraRules) &&
		s.last.ReviewDir == ag.ReviewDir &&
		s.last.BankBasename == ag.BankBasename &&
		s.last.AutoSubmit == ag.AutoSubmit {
		return ag
	}
	s.asr = NewASRClient(ag.ASR, ag.Language)
	s.llm = NewLLMClient(ag.LLM)
	s.prod = NewProducer(s.llm, ag.Language, ag.ExtraRules)
	if !ag.AutoSubmit {
		s.review = NewReviewPath(ag.ReviewDir, ag.BankBasename)
	} else {
		s.review = nil
	}
	s.last = ag
	s.have = true
	return ag
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Generate runs one interval. Operational outcomes (silence, ASR/LLM/dispatch
// failure, generation disabled) are folded into Result; a non-nil error means
// a programming bug.
func (s *Service) Generate(ctx context.Context, audio io.Reader, mime string) (Result, error) {
	ag := s.resolve()
	if !ag.Enabled {
		// Drain so the client connection finishes cleanly.
		_, _ = io.Copy(io.Discard, audio)
		return Result{Status: StatusSkipped, Reason: "auto_generation_disabled"}, nil
	}

	s.mu.Lock()
	asr, prod, review := s.asr, s.prod, s.review
	s.mu.Unlock()

	transcript, err := asr.Transcribe(ctx, audio, mime)
	if err != nil {
		s.failed(ctx, "asr_error", err)
		return Result{Status: StatusFailed, Reason: "asr_error"}, nil
	}

	transcriptWords := wordCount(transcript)
	if transcriptWords < silenceFloorWords {
		s.mu.Lock()
		buffered := s.words
		s.mu.Unlock()
		return Result{
			Status: StatusSkipped,
			Reason: "silence_or_too_short",
			Words:  buffered,
			Needed: ag.MinWordsPerQuestion,
		}, nil
	}

	s.mu.Lock()
	if s.accumulator.Len() > 0 {
		s.accumulator.WriteString(" ")
	}
	s.accumulator.WriteString(transcript)
	s.words += transcriptWords
	bufferedText := s.accumulator.String()
	bufferedWords := s.words
	s.mu.Unlock()

	needed := ag.MinWordsPerQuestion
	n := min(bufferedWords/needed, ag.MaxQuestionsPerPoll)
	if n < 1 {
		return Result{
			Status: StatusSkipped,
			Reason: "below_threshold",
			Words:  bufferedWords,
			Needed: needed,
		}, nil
	}

	bank, err := prod.Generate(ctx, bufferedText, n)
	if err != nil {
		s.failed(ctx, "llm_error", err)
		return Result{Status: StatusFailed, Reason: "llm_error"}, nil
	}
	if len(bank.Questions) == 0 {
		s.failed(ctx, "llm_error", errors.New("bank empty after validation"))
		return Result{Status: StatusFailed, Reason: "llm_error"}, nil
	}

	result := Result{Status: StatusGenerated, Questions: len(bank.Questions), AutoSubmit: ag.AutoSubmit}
	if ag.AutoSubmit {
		if s.dispatcher == nil {
			return Result{Status: StatusFailed, Reason: "dispatch_error"}, nil
		}
		if _, err := s.dispatcher.RunPollBank(ctx, bank, true); err != nil {
			s.failed(ctx, "dispatch_error", err)
			return Result{Status: StatusFailed, Reason: "dispatch_error"}, nil
		}
	} else {
		if review == nil {
			s.failed(ctx, "write_error", errors.New("review dir not configured"))
			return Result{Status: StatusFailed, Reason: "write_error"}, nil
		}
		path, err := review.Write(bank)
		if err != nil {
			s.failed(ctx, "write_error", err)
			return Result{Status: StatusFailed, Reason: "write_error"}, nil
		}
		result.BankPath = path
	}

	s.resetAccumulator()
	return result, nil
}

func (s *Service) failed(ctx context.Context, reason string, cause error) {
	slog.Warn("challenger: generation failed", "reason", reason, "err", cause)
	if s.sink != nil {
		s.sink.RecordGeneratorFailed(ctx, fmt.Sprintf("%s: %s", reason, cause))
	}
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}
