package challenges

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EligibleParticipant struct {
	DisplayName string
	Handle      string
	Language    string
}

type IssuedChallenge struct {
	ChallengeID string
	DisplayName string
	Question    Question
	Handle      string
	Language    string
	MessageRef  string
	IssuedAt    time.Time
}

type SkippedChallenge struct {
	ChallengeID   string
	DisplayName   string
	Reason        string
	AutoSubmitted bool
	SkippedAt     time.Time
}

// EventSink receives each challenge's outcome from the pipeline. The session
// coordinator implements it: it persists events and drives messenger replies.
type EventSink interface {
	RecordChallengeIssued(ctx context.Context, c IssuedChallenge) error
	RecordChallengeResult(ctx context.Context, challengeID, displayName string, result ScoreResult, submitted Answer, latencyMS int64) error
	RecordChallengeUnanswered(ctx context.Context, challengeID, displayName string) error
	RecordChallengeSkipped(ctx context.Context, sk SkippedChallenge) error

	NotifyAnswered(ctx context.Context, handle, lang, questionRef, replyRef string) error
	NotifyAnswerTimedOut(ctx context.Context, lang, ref string) error
}

// SendFn delivers one question and returns an opaque ref to the sent message.
type SendFn func(ctx context.Context, handle, lang, challengeID string, q Question) (ref string, err error)

type PollResult struct {
	PollID         string
	ScheduledCount int
	SkippedCount   int
}

type pendingChallenge struct {
	info     IssuedChallenge
	answerCh chan Answer
	cancel   context.CancelFunc
}

type Pipeline struct {
	sink EventSink

	mu      sync.Mutex
	pending map[string]*pendingChallenge
	wg      sync.WaitGroup
}

func NewPipeline(sink EventSink) *Pipeline {
	return &Pipeline{
		sink:    sink,
		pending: make(map[string]*pendingChallenge),
	}
}

// RunPoll assigns each eligible participant one random question from the
// bank, delivers it via send, and scores answers within answerWindow. The
// full bank is appended to meetingDir/questions.jsonl when meetingDir is
// non-empty.
func (p *Pipeline) RunPoll(
	ctx context.Context,
	bank Bank,
	challengeID string,
	answerWindow time.Duration,
	eligible []EligibleParticipant,
	send SendFn,
	meetingDir string,
) (PollResult, error) {
	if len(bank.Questions) == 0 {
		return PollResult{}, fmt.Errorf("challenges: empty bank")
	}
	res := PollResult{PollID: challengeID}
	if len(eligible) == 0 {
		slog.Info("challenges: poll skipped — no eligible participants", "challenge_id", challengeID)
		return res, nil
	}

	issuedAt := time.Now().UTC()

	selected := make([]Question, len(eligible))
	for i := range eligible {
		selected[i] = bank.Questions[rand.IntN(len(bank.Questions))] //nolint:gosec // G404: question selection is not security-sensitive
	}

	type deliveryResult struct{ delivered bool }
	results := make([]deliveryResult, len(eligible))

	var wg sync.WaitGroup
	for i, ep := range eligible {
		q := selected[i]
		wg.Go(func() {
			results[i].delivered = p.deliver(ctx, ep, challengeID, q, issuedAt, answerWindow, send)
		})
	}
	wg.Wait()

	for _, dr := range results {
		if dr.delivered {
			res.ScheduledCount++
		} else {
			res.SkippedCount++
		}
	}

	if meetingDir != "" {
		p.saveQuestions(bank, meetingDir)
	}

	return res, nil
}

// saveQuestions appends one JSON line per bank question to
// <meetingDir>/questions.jsonl, creating the meeting dir if needed.
func (p *Pipeline) saveQuestions(bank Bank, meetingDir string) {
	if err := os.MkdirAll(meetingDir, 0o755); err != nil {
		slog.Error("challenges: save questions: mkdir", "err", err)
		return
	}
	path := filepath.Join(meetingDir, QuestionsFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Error("challenges: save questions: open", "err", err)
		return
	}
	defer func() { _ = f.Close() }()

	for _, q := range bank.Questions {
		line, err := json.Marshal(q)
		if err != nil {
			slog.Error("challenges: save questions: marshal", "err", err)
			return
		}
		if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
			slog.Error("challenges: save questions: write", "err", err)
			return
		}
	}
}

func (p *Pipeline) deliver(
	ctx context.Context,
	ep EligibleParticipant,
	challengeID string,
	q Question,
	issuedAt time.Time,
	answerWindow time.Duration,
	send SendFn,
) bool {
	ref, err := send(ctx, ep.Handle, ep.Language, challengeID, q)
	if err != nil {
		slog.Warn("challenges: delivery failed", "participant", ep.DisplayName, "err", err)
		_ = p.sink.RecordChallengeSkipped(ctx, SkippedChallenge{
			ChallengeID:   challengeID,
			DisplayName:   ep.DisplayName,
			Reason:        "delivery_failed",
			AutoSubmitted: q.AutoSubmitted,
			SkippedAt:     time.Now().UTC(),
		})
		return false
	}

	issued := IssuedChallenge{
		ChallengeID: challengeID,
		DisplayName: ep.DisplayName,
		Question:    q,
		Handle:      ep.Handle,
		Language:    ep.Language,
		MessageRef:  ref,
		IssuedAt:    issuedAt,
	}
	if err := p.sink.RecordChallengeIssued(ctx, issued); err != nil {
		slog.Error("challenges: record issued", "err", err)
	}

	answerCh := make(chan Answer, 1)
	timeoutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), answerWindow)

	p.mu.Lock()
	p.pending[ep.Handle] = &pendingChallenge{info: issued, answerCh: answerCh, cancel: cancel}
	p.mu.Unlock()

	p.wg.Go(func() { //nolint:gosec // G118: derived ctx, bounded lifetime
		p.awaitAnswer(timeoutCtx, cancel, ep.Handle, issued, answerCh)
	})
	return true
}

func (p *Pipeline) awaitAnswer(ctx context.Context, cancel context.CancelFunc, handle string, issued IssuedChallenge, answerCh <-chan Answer) {
	defer cancel()
	defer func() {
		p.mu.Lock()
		delete(p.pending, handle)
		p.mu.Unlock()
	}()

	select {
	case answer, ok := <-answerCh:
		if !ok {
			return
		}
		latency := time.Since(issued.IssuedAt).Milliseconds()
		result := Score(issued.Question, answer)
		if err := p.sink.RecordChallengeResult(ctx, issued.ChallengeID, issued.DisplayName, result, answer, latency); err != nil {
			slog.Error("challenges: record result", "err", err)
		}
		if err := p.sink.NotifyAnswered(ctx, issued.Handle, issued.Language, issued.MessageRef, answer.MessageRef); err != nil {
			slog.Warn("challenges: acknowledge answer", "err", err)
		}

	case <-ctx.Done():
		bg := context.Background()
		if err := p.sink.RecordChallengeUnanswered(bg, issued.ChallengeID, issued.DisplayName); err != nil { //nolint:contextcheck
			slog.Error("challenges: record unanswered", "err", err)
		}
		if err := p.sink.NotifyAnswerTimedOut(bg, issued.Language, issued.MessageRef); err != nil { //nolint:contextcheck
			slog.Warn("challenges: mark question timed out", "err", err)
		}
	}
}

func (p *Pipeline) HandleAnswer(handle string, answer Answer) bool {
	p.mu.Lock()
	pc, ok := p.pending[handle]
	p.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case pc.answerCh <- answer:
		return true
	default:
		return false
	}
}

// Drain cancels all in-flight answer windows and waits for every
// awaitAnswer goroutine to finish so their unanswered events are
// written before the caller closes the event store.
func (p *Pipeline) Drain() {
	p.mu.Lock()
	for _, pc := range p.pending {
		pc.cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
