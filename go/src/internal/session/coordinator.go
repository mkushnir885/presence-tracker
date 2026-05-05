package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
)

// PairingPrefix is the prefix students must type in the meeting chat to pair
// their Telegram account. It is exported so messenger adapters can format the
// pairing code message without hardcoding this value.
const PairingPrefix = "PTRACK:"

// presenceInfo tracks the current in-meeting state of one participant.
type presenceInfo struct {
	participantID participants.ParticipantID
	displayName   string
	platformID    string
	joinedAt      time.Time
	lastChallenge time.Time // time of most-recent challenge; zero if none
}

// unregisteredInfo tracks a participant who joined but has not yet paired.
type unregisteredInfo struct {
	displayName string
	platformID  string
}

// CoordStatus is a snapshot of the coordinator's current state.
type CoordStatus struct {
	MeetingID    string
	Present      []PresenceStatus
	Unregistered []UnregisteredStatus
}

// PresenceStatus describes one registered participant who is currently in the meeting.
type PresenceStatus struct {
	ParticipantID string
	DisplayName   string
	PlatformID    string
	JoinedAt      time.Time
}

// UnregisteredStatus describes a participant who joined but has not completed pairing.
type UnregisteredStatus struct {
	DisplayName string
	PlatformID  string
}

// Config holds session-level configuration knobs.
type Config struct {
	MeetingID                   string // internal UUID written to every Parquet row
	PlatformMeetingID           string // provider-side meeting identifier (e.g. BBB external meeting ID)
	MeetingsDir                 string
	QuestionsDir                string
	ProviderName                string
	MessengerName               string
	AnswerWindowSecs            int
	MinGapBetweenChallengesSecs int
	EventStoreCompression       string
	RowGroupSize                int
}

// Coordinator orchestrates a single meeting session.
type Coordinator struct {
	cfg       Config
	provider  providers.Provider
	messenger messengers.Messenger
	registry  participants.Registry
	store     *eventstore.Writer
	poller    *challenges.Poller

	mu           sync.Mutex
	present      map[participants.ParticipantID]*presenceInfo
	unregistered map[string]*unregisteredInfo // platformID → info
}

// New creates a Coordinator. Call SetPoller before Run.
func New(cfg Config, provider providers.Provider, messenger messengers.Messenger, registry participants.Registry, store *eventstore.Writer) *Coordinator {
	return &Coordinator{
		cfg:          cfg,
		provider:     provider,
		messenger:    messenger,
		registry:     registry,
		store:        store,
		present:      make(map[participants.ParticipantID]*presenceInfo),
		unregistered: make(map[string]*unregisteredInfo),
	}
}

// SetPoller wires the challenge poller into the coordinator.
// It must be called before Run.
func (c *Coordinator) SetPoller(p *challenges.Poller) { c.poller = p }

// Run drives the session event loop. It returns when the meeting ends, ctx is
// cancelled, or an unrecoverable error occurs.
func (c *Coordinator) Run(ctx context.Context) error {
	providerEvents, err := c.provider.Subscribe(ctx, c.cfg.PlatformMeetingID)
	if err != nil {
		return fmt.Errorf("session: subscribe to provider: %w", err)
	}

	messengerEvents, err := c.messenger.Start(ctx)
	if err != nil {
		return fmt.Errorf("session: start messenger: %w", err)
	}

	defer func() { //nolint:contextcheck // ctx is cancelled by this point; cleanup uses a fresh context
		if c.poller != nil {
			c.poller.Drain()
		}
		bg := context.Background()
		c.writeEvent(bg, eventstore.Record{
			EventType: "meeting_ended",
			Source:    "system",
		})
		if err := c.store.Close(bg); err != nil {
			slog.Error("session: close eventstore", "err", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case evt, ok := <-providerEvents:
			if !ok {
				slog.Info("session: provider channel closed — meeting ended")
				return nil
			}
			c.handleProviderEvent(ctx, evt)

		case evt, ok := <-messengerEvents:
			if !ok {
				slog.Warn("session: messenger channel closed unexpectedly")
				continue
			}
			c.handleMessengerEvent(ctx, evt)
		}
	}
}

func (c *Coordinator) handleProviderEvent(ctx context.Context, evt providers.Event) {
	switch evt.Kind {
	case providers.EventKindParticipantJoined:
		c.onJoin(ctx, evt)
	case providers.EventKindParticipantLeft:
		c.onLeave(ctx, evt)
	case providers.EventKindChatMessage:
		c.onChatMessage(ctx, evt)
	case providers.EventKindMeetingEnded:
		// The provider channel will close; no extra action needed here.
	case providers.EventKindMeetingStarted:
		c.onMeetingStarted(ctx, evt)
	}
}

// onMeetingStarted records the meeting_started event using the provider's authoritative
// timestamp and updates the event store's start time so the output file is named correctly.
func (c *Coordinator) onMeetingStarted(ctx context.Context, evt providers.Event) {
	c.store.SetStartTime(evt.Timestamp)
	c.writeEvent(ctx, eventstore.Record{
		Timestamp: evt.Timestamp,
		EventType: "meeting_started",
		Source:    "provider:" + c.provider.Name(),
		Metadata:  map[string]string{"platform": c.provider.Name()},
	})
}

func (c *Coordinator) onJoin(ctx context.Context, evt providers.Event) {
	pid, known := c.registry.Resolve(c.provider.Name(), evt.PlatformID)

	rec := eventstore.Record{
		EventType:      "participant_joined",
		Source:         "provider:" + c.provider.Name(),
		PlatformHandle: evt.PlatformID,
		DisplayName:    evt.DisplayName,
		Metadata:       evt.Extra,
	}

	if known {
		rec.ParticipantID = string(pid)
		c.mu.Lock()
		c.present[pid] = &presenceInfo{
			participantID: pid,
			displayName:   evt.DisplayName,
			platformID:    evt.PlatformID,
			joinedAt:      evt.Timestamp,
		}
		c.mu.Unlock()
	} else {
		slog.Info("session: unregistered participant joined", "name", evt.DisplayName)
		c.mu.Lock()
		c.unregistered[evt.PlatformID] = &unregisteredInfo{
			displayName: evt.DisplayName,
			platformID:  evt.PlatformID,
		}
		c.mu.Unlock()
		c.writeEvent(ctx, eventstore.Record{
			EventType:      "participant_unregistered",
			Source:         "provider:" + c.provider.Name(),
			PlatformHandle: evt.PlatformID,
			DisplayName:    evt.DisplayName,
		})
	}

	c.writeEvent(ctx, rec)
}

func (c *Coordinator) onLeave(ctx context.Context, evt providers.Event) {
	pid, known := c.registry.Resolve(c.provider.Name(), evt.PlatformID)
	rec := eventstore.Record{
		EventType:      "participant_left",
		Source:         "provider:" + c.provider.Name(),
		PlatformHandle: evt.PlatformID,
	}
	c.mu.Lock()
	if known {
		rec.ParticipantID = string(pid)
		delete(c.present, pid)
	}
	delete(c.unregistered, evt.PlatformID)
	c.mu.Unlock()
	c.writeEvent(ctx, rec)
}

// Status returns a snapshot of the current session state.
func (c *Coordinator) Status() CoordStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	present := make([]PresenceStatus, 0, len(c.present))
	for _, info := range c.present {
		present = append(present, PresenceStatus{
			ParticipantID: string(info.participantID),
			DisplayName:   info.displayName,
			PlatformID:    info.platformID,
			JoinedAt:      info.joinedAt,
		})
	}

	unreg := make([]UnregisteredStatus, 0, len(c.unregistered))
	for _, info := range c.unregistered {
		unreg = append(unreg, UnregisteredStatus{
			DisplayName: info.displayName,
			PlatformID:  info.platformID,
		})
	}

	return CoordStatus{
		MeetingID:    c.cfg.MeetingID,
		Present:      present,
		Unregistered: unreg,
	}
}

func (c *Coordinator) onChatMessage(ctx context.Context, evt providers.Event) {
	// Scan for pairing code. Chat content itself is never stored.
	text := strings.TrimSpace(evt.Text)
	if !strings.HasPrefix(text, PairingPrefix) {
		return
	}
	code := strings.TrimPrefix(text, PairingPrefix)
	code = strings.Fields(code)[0] // take first token in case of trailing text

	pid, err := c.registry.CompletePairing(ctx, c.provider.Name(), evt.PlatformID, code)
	if err != nil {
		slog.Warn("session: pairing failed", "code", code, "err", err)
		return
	}

	slog.Info("session: participant paired", "id", pid, "platform", evt.PlatformID)
	c.writeEvent(ctx, eventstore.Record{
		EventType:      "participant_paired",
		Source:         "provider:" + c.provider.Name(),
		ParticipantID:  string(pid),
		PlatformHandle: evt.PlatformID,
		Metadata:       map[string]string{"messenger": c.messenger.Name(), "platform": c.provider.Name()},
	})

	c.mu.Lock()
	if info, ok := c.present[pid]; ok {
		info.participantID = pid
	}
	c.mu.Unlock()
}

func (c *Coordinator) handleMessengerEvent(_ context.Context, evt messengers.Event) {
	switch evt.Kind {
	case messengers.EventKindPairingStarted:
		slog.Info("session: pairing started", "handle", evt.Handle)
	case messengers.EventKindAnswerReceived:
		if c.poller == nil {
			return
		}
		answer := challenges.Answer{
			Text:       evt.Answer,
			Selected:   evt.Selected,
			MessageRef: evt.AnswerMessageRef.Opaque,
		}
		if !c.poller.HandleAnswer(evt.ChallengeID, answer) {
			slog.Debug("session: answer arrived after window closed", "challenge", evt.ChallengeID)
		}
	}
}

// TriggerPoll starts a poll round using the active poller. It is a no-op when
// no poller is set. Called by the auto-poll goroutine in track and by
// TriggerPollWithBank after installing a new poller.
func (c *Coordinator) TriggerPoll(ctx context.Context) error {
	if c.poller == nil {
		return nil
	}
	eligible := c.eligibleParticipants()
	if len(eligible) == 0 {
		return nil
	}
	sendFn := func(ctx context.Context, handle, challengeID string, q challenges.Question) (string, error) {
		mp := messengers.ChallengePrompt{
			ChallengeID:  challengeID,
			QuestionID:   q.QuestionID,
			Prompt:       q.Prompt,
			QuestionType: q.QuestionType,
			Choices:      q.Choices,
		}
		ref, err := c.messenger.SendChallenge(ctx, handle, mp)
		if err != nil {
			return "", err
		}
		return ref.Opaque, nil
	}
	return c.poller.TriggerPoll(ctx, eligible, c.cfg.QuestionsDir, c.cfg.MeetingID, sendFn)
}

// TriggerPollWithBank loads a question bank, installs a fresh poller, and
// immediately runs a poll round. Any previously pending poll is drained first.
// Called by the control socket handler when ptrack poll requests a round.
func (c *Coordinator) TriggerPollWithBank(ctx context.Context, loadBank BankLoader, bankPath string) error {
	ct, err := loadBank(bankPath)
	if err != nil {
		return fmt.Errorf("load bank: %w", err)
	}
	window := time.Duration(c.cfg.AnswerWindowSecs) * time.Second
	if c.poller != nil {
		c.poller.Drain()
	}
	c.poller = challenges.NewPoller(ct, c, window)
	return c.TriggerPoll(ctx)
}

// eligibleParticipants returns participants currently in the meeting, paired
// with the active messenger, and outside the challenge cooldown.
func (c *Coordinator) eligibleParticipants() []challenges.EligibleParticipant {
	minGap := time.Duration(c.cfg.MinGapBetweenChallengesSecs) * time.Second
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var out []challenges.EligibleParticipant
	for pid, info := range c.present {
		handle, ok := c.registry.Handle(pid, c.messenger.Name())
		if !ok {
			continue // not paired with active messenger
		}
		if !info.lastChallenge.IsZero() && now.Sub(info.lastChallenge) < minGap {
			continue // within cooldown
		}
		out = append(out, challenges.EligibleParticipant{
			ParticipantID: string(pid),
			Handle:        string(handle),
		})
	}
	return out
}

// writeEvent stamps and appends one event record to the event store.
// If r.Timestamp is already set (non-zero), it is preserved; otherwise time.Now() is used.
func (c *Coordinator) writeEvent(_ context.Context, r eventstore.Record) {
	r.EventID = uuid.Must(uuid.NewV7()).String()
	r.MeetingID = c.cfg.MeetingID
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	c.store.Append(r)
}

// RecordChallengeIssued implements challenges.EventSink.
func (c *Coordinator) RecordChallengeIssued(ctx context.Context, issued challenges.IssuedChallenge) error {
	c.writeEvent(ctx, eventstore.Record{
		EventType:     "challenge_issued",
		Source:        "scheduler",
		ParticipantID: issued.ParticipantID,
		Metadata: map[string]string{
			"challenge_id":    issued.ChallengeID,
			"challenge_type":  c.poller.ChallengeTypeName(),
			"question_id":     issued.Question.QuestionID,
			"answer_window_s": fmt.Sprintf("%d", c.cfg.AnswerWindowSecs),
		},
	})
	pid := participants.ParticipantID(issued.ParticipantID)
	c.mu.Lock()
	if info, ok := c.present[pid]; ok {
		info.lastChallenge = time.Now()
	}
	c.mu.Unlock()
	return nil
}

// RecordChallengeResult implements challenges.EventSink.
func (c *Coordinator) RecordChallengeResult(ctx context.Context, challengeID string, result challenges.ScoreResult, latencyMS int64) error {
	evtType := "challenge_answered_correct"
	if result == challenges.ScoreIncorrect {
		evtType = "challenge_answered_incorrect"
	}
	c.writeEvent(ctx, eventstore.Record{
		EventType: evtType,
		Source:    "messenger:" + c.messenger.Name(),
		Metadata: map[string]string{
			"challenge_id": challengeID,
			"latency_ms":   fmt.Sprintf("%d", latencyMS),
		},
	})
	return nil
}

// RecordChallengeUnanswered implements challenges.EventSink.
func (c *Coordinator) RecordChallengeUnanswered(ctx context.Context, challengeID string) error {
	c.writeEvent(ctx, eventstore.Record{
		EventType: "challenge_unanswered",
		Source:    "scheduler",
		Metadata:  map[string]string{"challenge_id": challengeID},
	})
	return nil
}

// RecordChallengeSkipped implements challenges.EventSink.
func (c *Coordinator) RecordChallengeSkipped(ctx context.Context, participantID, reason string) error {
	evtType := "challenge_skipped_offline"
	if reason == "unregistered" {
		evtType = "challenge_skipped_unregistered"
	}
	c.writeEvent(ctx, eventstore.Record{
		EventType:     evtType,
		Source:        "scheduler",
		ParticipantID: participantID,
	})
	return nil
}

// DeleteMessage implements challenges.EventSink.
func (c *Coordinator) DeleteMessage(ctx context.Context, ref string) error {
	return c.messenger.DeleteMessage(ctx, messengers.MessageRef{Opaque: ref})
}

// BankLoader creates a ChallengeType from a question-bank file path.
// It is passed to Listen so the session package stays decoupled from concrete
// challenge implementations.
type BankLoader func(bankPath string) (challenges.ChallengeType, error)

type controlRequest struct {
	BankPath string `json:"bank_path"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Listen starts a Unix domain socket at socketPath and accepts poll-trigger
// requests for the lifetime of ctx. Each request loads a question bank via
// loadBank and calls TriggerPoll.
//
// The listener is started in a background goroutine; Listen returns after the
// socket is bound so the caller can proceed with Run. A failed bind is logged
// as a warning — it does not prevent tracking.
func (c *Coordinator) Listen(ctx context.Context, socketPath string, loadBank BankLoader) {
	_ = os.Remove(socketPath)                 // remove stale socket from a previous run
	ln, err := net.Listen("unix", socketPath) //nolint:noctx // net.Listen is a synchronous bind; context cancellation is handled via the goroutine below
	if err != nil {
		slog.Warn("session: could not start control socket", "err", err)
		return
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener was closed
			}
			go c.handleControl(ctx, conn, loadBank)
		}
	}()

	slog.Info("session: control socket ready", "path", socketPath)
}

func (c *Coordinator) handleControl(ctx context.Context, conn net.Conn, loadBank BankLoader) {
	defer func() { _ = conn.Close() }()

	var req controlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: "invalid request: " + err.Error()}) //nolint:errchkjson // best-effort error response on a failing connection
		return
	}

	if err := c.TriggerPollWithBank(ctx, loadBank, req.BankPath); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: err.Error()}) //nolint:errchkjson // best-effort error response
		return
	}

	_ = json.NewEncoder(conn).Encode(controlResponse{OK: true}) //nolint:errchkjson // best-effort success response
}
