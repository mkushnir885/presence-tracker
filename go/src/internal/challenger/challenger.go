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
	Status    Status `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Questions int    `json:"questions,omitempty"`
	Words     int    `json:"words,omitempty"`
	Needed    int    `json:"needed,omitempty"`
}

// Dispatcher hands a generated bank straight to the running poll pipeline
// (the auto_submit path); implemented by the session coordinator.
type Dispatcher interface {
	RunPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error)
}

type EventSink interface {
	RecordGeneratorFailed(ctx context.Context, reason string)
}

// Service drives one session's auto-generation: it accumulates ASR
// transcripts across intervals and produces a question bank once enough
// speech has built up. Created per session.
type Service struct {
	cfg        config.AutoGenerationConfig
	asr        *ASRClient
	llm        *LLMClient
	producer   *Producer
	review     *ReviewDir
	dispatcher Dispatcher
	sink       EventSink

	mu          sync.Mutex
	accumulator strings.Builder
	words       int
}

func New(cfg config.AutoGenerationConfig, dispatcher Dispatcher, sink EventSink) *Service {
	s := &Service{
		cfg:        cfg,
		asr:        NewASRClient(cfg.ASR, cfg.Language),
		llm:        NewLLMClient(cfg.LLM),
		dispatcher: dispatcher,
		sink:       sink,
	}
	s.producer = NewProducer(s.llm, cfg.Language, cfg.ExtraRules)
	if !cfg.AutoSubmit {
		s.review = NewReviewDir(cfg.ReviewDir)
	}
	return s
}

func (s *Service) resetAccumulator() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulator.Reset()
	s.words = 0
}

func (s *Service) ReviewDirPath() string {
	if s.review == nil {
		return ""
	}
	return s.review.Dir()
}

func (s *Service) SweepReviewDir() error {
	if s.review == nil {
		return nil
	}
	return s.review.Sweep()
}

// Generate runs one interval: transcribe the audio, append it to the running
// transcript, and once enough words accumulate, ask the LLM for a bank and
// either dispatch it (auto_submit) or write it to the review dir. Operational
// outcomes (silence, ASR/LLM/dispatch failure) are folded into Result; a
// non-nil error means a programming bug.
func (s *Service) Generate(ctx context.Context, audio io.Reader, mime string) (Result, error) {
	transcript, err := s.asr.Transcribe(ctx, audio, mime)
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
			Needed: s.cfg.MinWordsPerQuestion,
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

	needed := s.cfg.MinWordsPerQuestion
	n := min(bufferedWords/needed, s.cfg.MaxQuestionsPerPoll)
	if n < 1 {
		return Result{
			Status: StatusSkipped,
			Reason: "below_threshold",
			Words:  bufferedWords,
			Needed: needed,
		}, nil
	}

	bank, err := s.producer.Generate(ctx, bufferedText, n)
	if err != nil {
		s.failed(ctx, "llm_error", err)
		return Result{Status: StatusFailed, Reason: "llm_error"}, nil
	}
	if len(bank.Questions) == 0 {
		s.failed(ctx, "llm_error", errors.New("bank empty after validation"))
		return Result{Status: StatusFailed, Reason: "llm_error"}, nil
	}

	if s.cfg.AutoSubmit {
		if s.dispatcher == nil {
			return Result{Status: StatusFailed, Reason: "dispatch_error"}, nil
		}
		if _, err := s.dispatcher.RunPollBank(ctx, bank, true); err != nil {
			s.failed(ctx, "dispatch_error", err)
			return Result{Status: StatusFailed, Reason: "dispatch_error"}, nil
		}
	} else {
		if _, err := s.review.Write(bank); err != nil {
			s.failed(ctx, "write_error", err)
			return Result{Status: StatusFailed, Reason: "write_error"}, nil
		}
	}

	s.resetAccumulator()
	return Result{Status: StatusGenerated, Questions: len(bank.Questions)}, nil
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
