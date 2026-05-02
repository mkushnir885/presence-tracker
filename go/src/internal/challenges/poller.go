package challenges

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"presence-tracker/src/internal/eventstore"
)

// EligibleParticipant is a snapshot of one participant eligible for a poll round.
type EligibleParticipant struct {
	ParticipantID string // internal stable ID
	Handle        string // messenger-specific contact identifier
}

// IssuedChallenge records the state of one delivered challenge.
type IssuedChallenge struct {
	ChallengeID   string
	ParticipantID string
	Question      Question
	Handle        string
	MessageRef    string // opaque; returned by messenger.SendChallenge
	IssuedAt      time.Time
}

// EventSink receives scored challenge results from the Poller.
// It is implemented by session.Coordinator.
type EventSink interface {
	RecordChallengeIssued(ctx context.Context, c IssuedChallenge) error
	RecordChallengeResult(ctx context.Context, challengeID string, result ScoreResult, latencyMS int64) error
	RecordChallengeUnanswered(ctx context.Context, challengeID string) error
	RecordChallengeSkipped(ctx context.Context, participantID, reason string) error
	// DeleteMessage deletes a messenger message by its opaque ref.
	DeleteMessage(ctx context.Context, ref string) error
}

// pendingChallenge tracks one outstanding challenge awaiting an answer.
type pendingChallenge struct {
	info     IssuedChallenge
	answerCh chan Answer // buffered(1); closed on timeout
	cancel   context.CancelFunc
}

// Poller coordinates challenge poll rounds for a session.
type Poller struct {
	challengeType ChallengeType
	sink          EventSink
	answerWindow  time.Duration

	mu      sync.Mutex
	pending map[string]*pendingChallenge // challengeID → pending
}

// NewPoller creates a Poller for the given ChallengeType.
func NewPoller(ct ChallengeType, sink EventSink, answerWindow time.Duration) *Poller {
	return &Poller{
		challengeType: ct,
		sink:          sink,
		answerWindow:  answerWindow,
		pending:       make(map[string]*pendingChallenge),
	}
}

// TriggerPoll runs one poll round: generates questions, delivers them
// simultaneously to all eligible participants, saves questions to JSONL,
// and starts per-challenge answer-window goroutines.
//
// Delivery is fire-and-forget per participant: a failed delivery is logged
// and skipped rather than aborting the whole poll.
func (p *Poller) TriggerPoll(ctx context.Context, eligible []EligibleParticipant, questionsDir, meetingID string, sendFn SendFn) error {
	if len(eligible) == 0 {
		slog.Info("poller: poll skipped — no eligible participants")
		return nil
	}

	questions, err := p.challengeType.Generate(ctx, len(eligible))
	if err != nil {
		return fmt.Errorf("poller: generate: %w", err)
	}
	if len(questions) == 0 {
		slog.Warn("poller: challenge type produced no questions")
		return nil
	}

	issuedAt := time.Now().UTC()

	var wg sync.WaitGroup
	for i, ep := range eligible {
		q := questions[i%len(questions)]
		cid := uuid.Must(uuid.NewV7()).String()

		wg.Go(func() {
			p.deliver(ctx, ep, cid, q, issuedAt, sendFn)
		})
	}
	wg.Wait()

	if questionsDir != "" && meetingID != "" {
		p.saveQuestions(questions, questionsDir, meetingID, issuedAt)
	}

	return nil
}

func (p *Poller) saveQuestions(questions []Question, questionsDir, meetingID string, issuedAt time.Time) {
	seen := make(map[string]bool, len(questions))
	records := make([]eventstore.QuestionRecord, 0, len(questions))
	ts := issuedAt.Format(time.RFC3339)
	for _, q := range questions {
		if seen[q.QuestionID] {
			continue
		}
		seen[q.QuestionID] = true
		records = append(records, eventstore.QuestionRecord{
			QuestionID:    q.QuestionID,
			ChallengeType: p.challengeType.Name(),
			QuestionType:  q.QuestionType,
			Prompt:        q.Prompt,
			Choices:       q.Choices,
			CorrectAnswer: q.Answer,
			MatchMode:     q.MatchMode,
			Tolerance:     q.Tolerance,
			IssuedAt:      ts,
		})
	}
	if err := eventstore.AppendQuestions(questionsDir, meetingID, records); err != nil {
		slog.Error("poller: save questions", "err", err)
	}
}

// deliver sends one challenge to one participant and starts the answer-window goroutine.
func (p *Poller) deliver(ctx context.Context, ep EligibleParticipant, cid string, q Question, issuedAt time.Time, sendFn SendFn) {
	ref, err := sendFn(ctx, ep.Handle, cid, q)
	if err != nil {
		slog.Warn("poller: delivery failed", "participant", ep.ParticipantID, "err", err)
		_ = p.sink.RecordChallengeSkipped(ctx, ep.ParticipantID, "delivery_failed")
		return
	}

	issued := IssuedChallenge{
		ChallengeID:   cid,
		ParticipantID: ep.ParticipantID,
		Question:      q,
		Handle:        ep.Handle,
		MessageRef:    ref,
		IssuedAt:      issuedAt,
	}

	if err := p.sink.RecordChallengeIssued(ctx, issued); err != nil {
		slog.Error("poller: record issued", "err", err)
	}

	answerCh := make(chan Answer, 1)
	timeoutCtx, cancel := context.WithTimeout(ctx, p.answerWindow)

	p.mu.Lock()
	p.pending[cid] = &pendingChallenge{info: issued, answerCh: answerCh, cancel: cancel}
	p.mu.Unlock()

	go p.awaitAnswer(timeoutCtx, cancel, cid, issued, answerCh) //nolint:gosec // G118: timeoutCtx is derived from the request context; goroutine lifetime is bounded by the answer window
}

func (p *Poller) awaitAnswer(ctx context.Context, cancel context.CancelFunc, cid string, issued IssuedChallenge, answerCh <-chan Answer) {
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
		result := p.challengeType.Score(issued.Question, answer)
		if err := p.sink.RecordChallengeResult(ctx, cid, result, latency); err != nil {
			slog.Error("poller: record result", "err", err)
		}
		if err := p.sink.DeleteMessage(ctx, issued.MessageRef); err != nil {
			slog.Warn("poller: delete question message", "err", err)
		}
		if answer.MessageRef != "" {
			if err := p.sink.DeleteMessage(ctx, answer.MessageRef); err != nil {
				slog.Warn("poller: delete answer message", "err", err)
			}
		}

	case <-ctx.Done():
		// ctx is the answer-window timer; cleanup must proceed after it expires.
		bg := context.Background()
		if err := p.sink.RecordChallengeUnanswered(bg, cid); err != nil { //nolint:contextcheck
			slog.Error("poller: record unanswered", "err", err)
		}
		if err := p.sink.DeleteMessage(bg, issued.MessageRef); err != nil { //nolint:contextcheck
			slog.Warn("poller: delete question message on timeout", "err", err)
		}
	}
}

// HandleAnswer routes an incoming answer to the pending challenge, if any.
// Returns false if no matching pending challenge exists (already timed out).
func (p *Poller) HandleAnswer(challengeID string, answer Answer) bool {
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
		// Channel already has an answer (race); ignore duplicate.
		return false
	}
}

// ChallengeTypeName returns the name of the underlying ChallengeType.
func (p *Poller) ChallengeTypeName() string { return p.challengeType.Name() }

// Drain cancels all pending challenges (called on session end).
func (p *Poller) Drain() {
	p.mu.Lock()
	for _, pc := range p.pending {
		pc.cancel()
	}
	p.mu.Unlock()
}

// SendFn is the function the poller calls to deliver one challenge via the
// messenger. It returns an opaque MessageRef string.
type SendFn func(ctx context.Context, handle, challengeID string, q Question) (ref string, err error)
