package session

import (
	"context"
	"fmt"
	"log/slog"
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

// presenceInfo tracks the current in-meeting state of one verified participant.
type presenceInfo struct {
	participantID participants.ParticipantID
	displayName   string
	platformID    string
	joinedAt      time.Time
	lastChallenge time.Time
}

// pendingVerification tracks a participant who joined and for whom a
// confirmation DM has been sent but not yet answered.
type pendingVerification struct {
	participantID participants.ParticipantID
	displayName   string
	platformID    string
	joinedAt      time.Time
}

// unregisteredInfo tracks a participant who joined but has no registry entry.
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

// PresenceStatus describes one verified participant currently in the meeting.
type PresenceStatus struct {
	ParticipantID string
	DisplayName   string
	PlatformID    string
	JoinedAt      time.Time
}

// UnregisteredStatus describes a participant who joined but is not in the registry.
type UnregisteredStatus struct {
	DisplayName string
	PlatformID  string
}

// Config holds session-level configuration knobs.
type Config struct {
	MeetingID                   string
	PlatformMeetingID           string
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
	pipeline  *challenges.Pipeline

	mu           sync.Mutex
	present      map[participants.ParticipantID]*presenceInfo
	pending      map[string]*pendingVerification // messengerHandle → info
	unregistered map[string]*unregisteredInfo    // platformID → info
}

// New creates a Coordinator.
func New(cfg Config, provider providers.Provider, messenger messengers.Messenger, registry participants.Registry, store *eventstore.Writer) *Coordinator {
	c := &Coordinator{
		cfg:          cfg,
		provider:     provider,
		messenger:    messenger,
		registry:     registry,
		store:        store,
		present:      make(map[participants.ParticipantID]*presenceInfo),
		pending:      make(map[string]*pendingVerification),
		unregistered: make(map[string]*unregisteredInfo),
	}
	window := time.Duration(cfg.AnswerWindowSecs) * time.Second
	c.pipeline = challenges.NewPipeline(c, window)
	return c
}

// MeetingID returns the internal meeting ID of this session.
func (c *Coordinator) MeetingID() string { return c.cfg.MeetingID }

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
		c.pipeline.Drain()
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
	case providers.EventKindMeetingEnded:
		// Provider channel will close; no extra action needed.
	case providers.EventKindMeetingStarted:
		c.onMeetingStarted(ctx, evt)
	}
}

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
	rec := eventstore.Record{
		EventType:      "participant_joined",
		Source:         "provider:" + c.provider.Name(),
		PlatformHandle: evt.PlatformID,
		DisplayName:    evt.DisplayName,
		Metadata:       evt.Extra,
	}

	pid, known := c.registry.Resolve(c.provider.Name(), evt.DisplayName)
	if known {
		rec.ParticipantID = string(pid)
		handle, hasHandle := c.registry.Handle(pid, c.messenger.Name())
		if hasHandle {
			c.writeEvent(ctx, rec)
			c.sendVerification(ctx, pid, handle, evt)
			return
		}
		// Registered but no handle for the active messenger: treat as unregistered.
	}

	c.writeEvent(ctx, rec)
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

func (c *Coordinator) sendVerification(ctx context.Context, pid participants.ParticipantID, handle participants.Handle, evt providers.Event) {
	c.writeEvent(ctx, eventstore.Record{
		EventType:      "participant_verification_sent",
		Source:         "messenger:" + c.messenger.Name(),
		ParticipantID:  string(pid),
		PlatformHandle: evt.PlatformID,
		DisplayName:    evt.DisplayName,
		Metadata:       map[string]string{"messenger": c.messenger.Name(), "platform": c.provider.Name()},
	})

	if _, err := c.messenger.SendJoinConfirmation(ctx, string(handle), c.cfg.MeetingID, c.provider.Name()); err != nil {
		slog.Warn("session: send join confirmation", "err", err)
		// Fall back to unregistered state so challenges aren't silently skipped.
		c.mu.Lock()
		c.unregistered[evt.PlatformID] = &unregisteredInfo{displayName: evt.DisplayName, platformID: evt.PlatformID}
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	c.pending[string(handle)] = &pendingVerification{
		participantID: pid,
		displayName:   evt.DisplayName,
		platformID:    evt.PlatformID,
		joinedAt:      evt.Timestamp,
	}
	c.mu.Unlock()
}

func (c *Coordinator) onLeave(ctx context.Context, evt providers.Event) {
	rec := eventstore.Record{
		EventType:      "participant_left",
		Source:         "provider:" + c.provider.Name(),
		PlatformHandle: evt.PlatformID,
	}

	c.mu.Lock()
	// Find verified participant by platformID.
	for pid, info := range c.present {
		if info.platformID == evt.PlatformID {
			rec.ParticipantID = string(pid)
			delete(c.present, pid)
			break
		}
	}
	// Clean up any pending verification for this platform ID.
	for handle, pv := range c.pending {
		if pv.platformID == evt.PlatformID {
			delete(c.pending, handle)
			break
		}
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

func (c *Coordinator) handleMessengerEvent(ctx context.Context, evt messengers.Event) {
	switch evt.Kind {
	case messengers.EventKindJoinConfirmation:
		c.onJoinConfirmation(ctx, evt)
	case messengers.EventKindRegistration:
		c.onRegistration(ctx, evt)
	case messengers.EventKindAnswerReceived:
		answer := challenges.Answer{
			Text:       evt.Answer,
			Selected:   evt.Selected,
			MessageRef: evt.AnswerMessageRef.Opaque,
		}
		if !c.pipeline.HandleAnswer(evt.ChallengeID, answer) {
			slog.Debug("session: answer arrived after window closed", "challenge", evt.ChallengeID)
		}
	}
}

func (c *Coordinator) onJoinConfirmation(ctx context.Context, evt messengers.Event) {
	c.mu.Lock()
	pv, ok := c.pending[evt.Handle]
	if ok {
		delete(c.pending, evt.Handle)
	}
	c.mu.Unlock()

	if !ok {
		return // stale confirmation (e.g. from a previous meeting)
	}

	if evt.Confirmed {
		latency := evt.Timestamp.Sub(pv.joinedAt).Milliseconds()
		c.mu.Lock()
		c.present[pv.participantID] = &presenceInfo{
			participantID: pv.participantID,
			displayName:   pv.displayName,
			platformID:    pv.platformID,
			joinedAt:      pv.joinedAt,
		}
		c.mu.Unlock()
		slog.Info("session: participant verified", "id", pv.participantID, "name", pv.displayName)
		c.writeEvent(ctx, eventstore.Record{
			EventType:      "participant_verified",
			Source:         "messenger:" + c.messenger.Name(),
			ParticipantID:  string(pv.participantID),
			PlatformHandle: pv.platformID,
			DisplayName:    pv.displayName,
			Metadata: map[string]string{
				"messenger":  c.messenger.Name(),
				"platform":   c.provider.Name(),
				"latency_ms": fmt.Sprintf("%d", latency),
			},
		})
	} else {
		slog.Info("session: participant denied", "name", pv.displayName)
		c.writeEvent(ctx, eventstore.Record{
			EventType:      "participant_verification_denied",
			Source:         "messenger:" + c.messenger.Name(),
			ParticipantID:  string(pv.participantID),
			PlatformHandle: pv.platformID,
			DisplayName:    pv.displayName,
			Metadata:       map[string]string{"messenger": c.messenger.Name(), "platform": c.provider.Name()},
		})
	}
}

// onRegistration handles a /register event from the messenger. If the newly
// registered participant is already present in the meeting as unregistered,
// a verification DM is sent immediately.
func (c *Coordinator) onRegistration(ctx context.Context, evt messengers.Event) {
	if evt.Platform != c.provider.Name() {
		return // registered for a different platform; irrelevant to this session
	}

	c.mu.Lock()
	var unregInfo *unregisteredInfo
	for _, info := range c.unregistered {
		if strings.EqualFold(strings.TrimSpace(info.displayName), strings.TrimSpace(evt.DisplayName)) {
			unregInfo = info
			delete(c.unregistered, info.platformID)
			break
		}
	}
	c.mu.Unlock()

	if unregInfo == nil {
		return // not currently in the meeting
	}

	pid, known := c.registry.Resolve(c.provider.Name(), evt.DisplayName)
	if !known {
		return // shouldn't happen right after a successful /register
	}
	handle, hasHandle := c.registry.Handle(pid, c.messenger.Name())
	if !hasHandle {
		return
	}

	joinEvt := providers.Event{
		PlatformID:  unregInfo.platformID,
		DisplayName: unregInfo.displayName,
		Timestamp:   time.Now().UTC(),
	}
	c.sendVerification(ctx, pid, handle, joinEvt)
}

// RunPoll loads a question bank from disk and dispatches one poll round
// to the eligible participants currently in the meeting. typeLabel is the
// free-form producer tag stamped onto every challenge_issued event for
// this round (e.g. "custom", "combined", "aigenerated").
func (c *Coordinator) RunPoll(ctx context.Context, bankPath, typeLabel string) (challenges.PollResult, error) {
	bank, err := challenges.Load(bankPath)
	if err != nil {
		return challenges.PollResult{}, err
	}
	eligible := c.eligibleParticipants()

	sendFn := func(ctx context.Context, handle, challengeID string, q challenges.Question) (string, error) {
		mp := messengers.ChallengePrompt{
			ChallengeID:  challengeID,
			QuestionID:   q.QuestionID,
			Prompt:       q.Prompt,
			QuestionType: string(q.QuestionType),
			Choices:      q.Choices,
		}
		ref, err := c.messenger.SendChallenge(ctx, handle, mp)
		if err != nil {
			return "", err
		}
		return ref.Opaque, nil
	}
	return c.pipeline.RunPoll(ctx, bank, typeLabel, eligible, sendFn, c.cfg.QuestionsDir, c.cfg.MeetingID)
}

// eligibleParticipants returns verified participants currently in the meeting,
// paired with the active messenger, and outside the challenge cooldown.
func (c *Coordinator) eligibleParticipants() []challenges.EligibleParticipant {
	minGap := time.Duration(c.cfg.MinGapBetweenChallengesSecs) * time.Second
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var out []challenges.EligibleParticipant
	for pid, info := range c.present {
		handle, ok := c.registry.Handle(pid, c.messenger.Name())
		if !ok {
			continue
		}
		if !info.lastChallenge.IsZero() && now.Sub(info.lastChallenge) < minGap {
			continue
		}
		out = append(out, challenges.EligibleParticipant{
			ParticipantID: string(pid),
			Handle:        string(handle),
		})
	}
	return out
}

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
			"challenge_type":  issued.TypeLabel,
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
