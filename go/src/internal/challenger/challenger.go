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

// silenceFloorWords drops ASR results shorter than this before appending
// to the accumulator. The cheapest defence against Whisper hallucinations
// ("Thanks for watching!", "Subtitles by ...") on silent input. Tunable
// only by editing this constant — it never made it into config.
const silenceFloorWords = 5

// Status is the outcome of one Generate call, surfaced to the browser as
// the JSON response body of POST /audio/segment.
type Status string

const (
	StatusGenerated Status = "generated"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
)

// Result is the response payload of POST /audio/segment.
type Result struct {
	Status    Status `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Questions int    `json:"questions,omitempty"`
	Words     int    `json:"words,omitempty"`
	Needed    int    `json:"needed,omitempty"`
}

// Dispatcher hands a generated bank to the running session's challenge
// pipeline. Implemented by *session.Coordinator's in-memory poll entry.
type Dispatcher interface {
	RunPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error)
}

// EventSink records the diagnostic events the challenger emits when
// generation fails. Implemented by *session.Coordinator.
type EventSink interface {
	RecordGeneratorFailed(ctx context.Context, reason string)
}

// Service drives one session's auto-generation pipeline. One Service is
// created when a session starts and discarded on session end — the
// accumulator is therefore inherently per-session.
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

// New constructs a Service. dispatcher and sink may both be nil, in which
// case Generate is degraded: dispatch errors when auto_submit is true,
// and failed-generation events are dropped. They are nil only in unit
// tests; cmd/ptrack always wires them.
func New(cfg config.AutoGenerationConfig, dispatcher Dispatcher, sink EventSink) *Service {
	s := &Service{
		cfg:        cfg,
		asr:        NewASRClient(cfg.ASR, cfg.Language),
		llm:        NewLLMClient(cfg.LLM),
		dispatcher: dispatcher,
		sink:       sink,
	}
	s.producer = NewProducer(s.llm, cfg.Language)
	if !cfg.AutoSubmit {
		s.review = NewReviewDir(cfg.ReviewDir)
	}
	return s
}

// ResetAccumulator clears any buffered transcript. Called at session
// start (sweep stale state) and indirectly on successful generation.
func (s *Service) ResetAccumulator() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulator.Reset()
	s.words = 0
}

// ReviewDirPath returns the configured review directory, or "" when
// auto_submit is true (in which case the challenger never writes to disk).
func (s *Service) ReviewDirPath() string {
	if s.review == nil {
		return ""
	}
	return s.review.Dir()
}

// SweepReviewDir removes every pending auto-*.yaml so a new session does
// not see stale files from a prior one. No-op when auto_submit is true.
func (s *Service) SweepReviewDir() error {
	if s.review == nil {
		return nil
	}
	return s.review.Sweep()
}

// Generate runs one cycle: ASR → accumulator append → optional LLM call
// → either in-memory dispatch or review-dir write. Returns a non-nil
// error only for programming bugs; every operational outcome (silence,
// below-threshold transcript, ASR/LLM/dispatch failure) is folded into
// Result so the HTTP handler can return 200 with a structured body.
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

	s.ResetAccumulator()
	return Result{Status: StatusGenerated, Questions: len(bank.Questions)}, nil
}

// failed logs the failure and (when wired) emits a
// challenge_generator_failed event so the teacher sees it in the system
// log. The accumulator is intentionally not cleared on failure — the
// content is still useful next interval.
func (s *Service) failed(ctx context.Context, reason string, cause error) {
	slog.Warn("challenger: generation failed", "reason", reason, "err", cause)
	if s.sink != nil {
		s.sink.RecordGeneratorFailed(ctx, fmt.Sprintf("%s: %s", reason, cause))
	}
}

// wordCount counts whitespace-separated tokens. Good enough for thresholding —
// no need to be tokenizer-accurate when the threshold itself is a guess.
func wordCount(s string) int {
	return len(strings.Fields(s))
}
