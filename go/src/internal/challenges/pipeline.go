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

type EligibleParticipant struct {
	DisplayName string
	Handle      string
	Language    string
}

type IssuedChallenge struct {
	ChallengeID   string
	DisplayName   string
	AutoSubmitted bool
	Question      Question
	Handle        string
	Language      string
	MessageRef    string
	IssuedAt      time.Time
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
	RecordChallengeResult(ctx context.Context, challengeID string, result ScoreResult, submitted Answer, latencyMS int64) error
	RecordChallengeUnanswered(ctx context.Context, challengeID string) error
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
	sink         EventSink
	answerWindow time.Duration

	mu      sync.Mutex
	pending map[string]*pendingChallenge
}

func NewPipeline(sink EventSink, answerWindow time.Duration) *Pipeline {
	return &Pipeline{
		sink:         sink,
		answerWindow: answerWindow,
		pending:      make(map[string]*pendingChallenge),
	}
}

// RunPoll assigns each eligible participant one random question from the
// bank, delivers it via send, and scores answers within answerWindow.
func (p *Pipeline) RunPoll(
	ctx context.Context,
	bank Bank,
	autoSubmitted bool,
	eligible []EligibleParticipant,
	send SendFn,
	questionsDir, fileBaseName string,
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

	// Each delivery gets a fresh QuestionID so the same bank question handed
	// to several participants is tracked as distinct issued instances.
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

	if questionsDir != "" && fileBaseName != "" {
		p.saveQuestions(assignments, autoSubmitted, questionsDir, fileBaseName, issuedAt)
	}

	return res, nil
}

func (p *Pipeline) saveQuestions(questions []Question, autoSubmitted bool, questionsDir, fileBaseName string, issuedAt time.Time) {
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
	if err := eventstore.AppendQuestions(questionsDir, fileBaseName, records); err != nil {
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
	ref, err := send(ctx, ep.Handle, ep.Language, cid, q)
	if err != nil {
		slog.Warn("challenges: delivery failed", "participant", ep.DisplayName, "err", err)
		_ = p.sink.RecordChallengeSkipped(ctx, SkippedChallenge{
			ChallengeID:   cid,
			DisplayName:   ep.DisplayName,
			Reason:        "delivery_failed",
			AutoSubmitted: autoSubmitted,
			SkippedAt:     time.Now().UTC(),
		})
		return false
	}

	issued := IssuedChallenge{
		ChallengeID:   cid,
		DisplayName:   ep.DisplayName,
		AutoSubmitted: autoSubmitted,
		Question:      q,
		Handle:        ep.Handle,
		Language:      ep.Language,
		MessageRef:    ref,
		IssuedAt:      issuedAt,
	}
	if err := p.sink.RecordChallengeIssued(ctx, issued); err != nil {
		slog.Error("challenges: record issued", "err", err)
	}

	answerCh := make(chan Answer, 1)
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
		if err := p.sink.NotifyAnswered(ctx, issued.Handle, issued.Language, issued.MessageRef, answer.MessageRef); err != nil {
			slog.Warn("challenges: acknowledge answer", "err", err)
		}

	case <-ctx.Done():
		bg := context.Background()
		if err := p.sink.RecordChallengeUnanswered(bg, cid); err != nil { //nolint:contextcheck
			slog.Error("challenges: record unanswered", "err", err)
		}
		if err := p.sink.NotifyAnswerTimedOut(bg, issued.Language, issued.MessageRef); err != nil { //nolint:contextcheck
			slog.Warn("challenges: mark question timed out", "err", err)
		}
	}
}

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

func (p *Pipeline) Drain() {
	p.mu.Lock()
	for _, pc := range p.pending {
		pc.cancel()
	}
	p.mu.Unlock()
}
