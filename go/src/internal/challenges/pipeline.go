package challenges

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"

	"presence-tracker/src/internal/eventstore"
)

// EligibleParticipant is a snapshot of one participant eligible to receive
// a challenge in a poll round. Identity is the canonical display name —
// the same value written to every per-participant Parquet event.
type EligibleParticipant struct {
	DisplayName string
	Handle      string
}

// IssuedChallenge records one delivered challenge.
type IssuedChallenge struct {
	ChallengeID   string
	DisplayName   string
	AutoSubmitted bool
	Question      Question
	Handle        string
	MessageRef    string
	IssuedAt      time.Time
}

// EventSink receives scored challenge results and side effects. Implemented
// by session.Coordinator.
type EventSink interface {
	RecordChallengeIssued(ctx context.Context, c IssuedChallenge) error
	RecordChallengeResult(ctx context.Context, challengeID string, result ScoreResult, submitted Answer, latencyMS int64) error
	RecordChallengeUnanswered(ctx context.Context, challengeID string) error
	RecordChallengeSkipped(ctx context.Context, displayName, reason string) error

	// NotifyAnswered acknowledges a received answer. It deletes the
	// question message and, for text/numeric answers, the user's reply,
	// then sends a fresh "answer saved" message to handle. replyRef is
	// empty for multiple-choice answers where the user tapped a button
	// instead of replying.
	NotifyAnswered(ctx context.Context, handle, questionRef, replyRef string) error
	// NotifyAnswerTimedOut edits the question message in place to show
	// that the answer window has closed without an answer being
	// recorded.
	NotifyAnswerTimedOut(ctx context.Context, ref string) error
}

// SendFn dispatches one challenge to one participant via the messenger.
type SendFn func(ctx context.Context, handle, challengeID string, q Question) (ref string, err error)

// PollResult summarizes a freshly scheduled poll round.
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

// Pipeline tracks outstanding challenges for one session. A single Pipeline
// is created per session and reused across poll rounds.
type Pipeline struct {
	sink         EventSink
	answerWindow time.Duration

	mu      sync.Mutex
	pending map[string]*pendingChallenge // challengeID → pending
}

// NewPipeline creates a Pipeline for the given session.
func NewPipeline(sink EventSink, answerWindow time.Duration) *Pipeline {
	return &Pipeline{
		sink:         sink,
		answerWindow: answerWindow,
		pending:      make(map[string]*pendingChallenge),
	}
}

// RunPoll dispatches one poll round: assigns one random question per
// eligible participant, appends issued questions to the meeting's .jsonl
// file, delivers them, and starts per-challenge answer-window goroutines.
//
// Returns immediately once dispatch has been scheduled. The caller must
// route incoming answers via HandleAnswer.
func (p *Pipeline) RunPoll(
	ctx context.Context,
	bank Bank,
	autoSubmitted bool,
	eligible []EligibleParticipant,
	send SendFn,
	questionsDir, meetingID string,
) (PollResult, error) {
	if len(bank.Questions) == 0 {
		return PollResult{}, fmt.Errorf("challenges: empty bank")
	}
	pollID := uuid.Must(uuid.NewV7()).String()
	res := PollResult{PollID: pollID}
	if len(eligible) == 0 {
		slog.Info("challenges: poll skipped — no eligible participants", "poll_id", pollID)
		return res, nil
	}

	issuedAt := time.Now().UTC()

	assignments := make([]Question, len(eligible))
	for i := range eligible {
		src := bank.Questions[rand.IntN(len(bank.Questions))] //nolint:gosec // G404: question selection is not security-sensitive
		assignments[i] = Question{
			QuestionID:   uuid.Must(uuid.NewV7()).String(),
			QuestionType: src.QuestionType,
			Prompt:       src.Prompt,
			Choices:      src.Choices,
			Answer:       src.Answer,
			MatchMode:    src.MatchMode,
			Tolerance:    src.Tolerance,
		}
	}

	type deliveryResult struct{ delivered bool }
	results := make([]deliveryResult, len(eligible))

	var wg sync.WaitGroup
	for i, ep := range eligible {
		q := assignments[i]
		cid := uuid.Must(uuid.NewV7()).String()
		wg.Go(func() {
			results[i].delivered = p.deliver(ctx, ep, cid, q, autoSubmitted, issuedAt, send)
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

	if questionsDir != "" && meetingID != "" {
		p.saveQuestions(assignments, autoSubmitted, questionsDir, meetingID, issuedAt)
	}

	return res, nil
}

func (p *Pipeline) saveQuestions(questions []Question, autoSubmitted bool, questionsDir, meetingID string, issuedAt time.Time) {
	ts := issuedAt.Format(time.RFC3339)
	records := make([]eventstore.QuestionRecord, 0, len(questions))
	for _, q := range questions {
		records = append(records, eventstore.QuestionRecord{
			QuestionID:    q.QuestionID,
			AutoSubmitted: autoSubmitted,
			QuestionType:  string(q.QuestionType),
			Prompt:        q.Prompt,
			Choices:       q.Choices,
			CorrectAnswer: q.Answer,
			MatchMode:     q.MatchMode,
			Tolerance:     q.Tolerance,
			IssuedAt:      ts,
		})
	}
	if err := eventstore.AppendQuestions(questionsDir, meetingID, records); err != nil {
		slog.Error("challenges: save questions", "err", err)
	}
}

func (p *Pipeline) deliver(
	ctx context.Context,
	ep EligibleParticipant,
	cid string,
	q Question,
	autoSubmitted bool,
	issuedAt time.Time,
	send SendFn,
) bool {
	ref, err := send(ctx, ep.Handle, cid, q)
	if err != nil {
		slog.Warn("challenges: delivery failed", "participant", ep.DisplayName, "err", err)
		_ = p.sink.RecordChallengeSkipped(ctx, ep.DisplayName, "delivery_failed")
		return false
	}

	issued := IssuedChallenge{
		ChallengeID:   cid,
		DisplayName:   ep.DisplayName,
		AutoSubmitted: autoSubmitted,
		Question:      q,
		Handle:        ep.Handle,
		MessageRef:    ref,
		IssuedAt:      issuedAt,
	}
	if err := p.sink.RecordChallengeIssued(ctx, issued); err != nil {
		slog.Error("challenges: record issued", "err", err)
	}

	answerCh := make(chan Answer, 1)
	// Detach from the caller's ctx (often an HTTP request that
	// finishes long before answer_window elapses) but keep its values
	// so logging/tracing remain consistent. Drain still cancels via
	// the stored cancel func when the session ends.
	timeoutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.answerWindow)

	p.mu.Lock()
	p.pending[cid] = &pendingChallenge{info: issued, answerCh: answerCh, cancel: cancel}
	p.mu.Unlock()

	go p.awaitAnswer(timeoutCtx, cancel, cid, issued, answerCh) //nolint:gosec // G118: derived ctx, bounded lifetime
	return true
}

func (p *Pipeline) awaitAnswer(ctx context.Context, cancel context.CancelFunc, cid string, issued IssuedChallenge, answerCh <-chan Answer) {
	defer cancel()
	defer func() {
		p.mu.Lock()
		delete(p.pending, cid)
		p.mu.Unlock()
	}()

	select {
	case answer, ok := <-answerCh:
		if !ok {
			return
		}
		latency := time.Since(issued.IssuedAt).Milliseconds()
		result := Score(issued.Question, answer)
		if err := p.sink.RecordChallengeResult(ctx, cid, result, answer, latency); err != nil {
			slog.Error("challenges: record result", "err", err)
		}
		if err := p.sink.NotifyAnswered(ctx, issued.Handle, issued.MessageRef, answer.MessageRef); err != nil {
			slog.Warn("challenges: acknowledge answer", "err", err)
		}

	case <-ctx.Done():
		bg := context.Background()
		if err := p.sink.RecordChallengeUnanswered(bg, cid); err != nil { //nolint:contextcheck
			slog.Error("challenges: record unanswered", "err", err)
		}
		if err := p.sink.NotifyAnswerTimedOut(bg, issued.MessageRef); err != nil { //nolint:contextcheck
			slog.Warn("challenges: mark question timed out", "err", err)
		}
	}
}

// HandleAnswer routes an incoming answer to its pending challenge. Returns
// false if no matching pending entry exists (already timed out or unknown).
func (p *Pipeline) HandleAnswer(challengeID string, answer Answer) bool {
	p.mu.Lock()
	pc, ok := p.pending[challengeID]
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

// Drain cancels all pending challenges (called on session end).
func (p *Pipeline) Drain() {
	p.mu.Lock()
	for _, pc := range p.pending {
		pc.cancel()
	}
	p.mu.Unlock()
}
