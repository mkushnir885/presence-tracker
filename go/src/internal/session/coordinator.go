package session

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
)

var _ challenges.EventSink = (*Coordinator)(nil)

// bufferedJoin holds a participant_joined that has not yet been flushed
// to Parquet because verification is still pending.
type bufferedJoin struct {
	platformID string
	joinedAt   time.Time
	metadata   map[string]string
}

// nameState owns the per-display-name lifecycle inside a session.
// Lookup keyed on normalize(canonicalName). A state is created when a
// registered participant first joins and destroyed when no platformID
// is currently claiming the name.
type nameState struct {
	canonicalName      string                // as stored in the registry, written verbatim to Parquet
	handle             string                // for sending and editing the verification DM
	platformIDs        map[string]struct{}   // every current claimant (verified, pending, or ignored)
	pending            *bufferedJoin         // buffered join awaiting verification, if any
	confirmRef         messengers.MessageRef // ref to the verification DM, for edit on collision
	verifiedPlatformID string                // platformID of the verified participant; "" if none
	verifiedAt         time.Time
	lastChallenge      time.Time
	tainted            bool // a pre-verification collision occurred; ignore everything until cleared
}

// CoordStatus is a snapshot of the coordinator's current state.
type CoordStatus struct {
	MeetingID    string
	Present      []PresenceStatus
	Unregistered []UnregisteredStatus
}

// PresenceStatus describes one verified participant currently in the meeting.
type PresenceStatus struct {
	DisplayName string
	PlatformID  string
	JoinedAt    time.Time
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

	mu            sync.Mutex
	names         map[string]*nameState      // normName → state
	platformIndex map[string]string          // platformID → normName (for fast onLeave)
	pendingHandle map[string]string          // messenger handle → normName (only while pending)
	unregistered  map[string]providers.Event // platformID → original join event (live GUI only)
}

// New creates a Coordinator.
func New(cfg Config, provider providers.Provider, messenger messengers.Messenger, registry participants.Registry, store *eventstore.Writer) *Coordinator {
	c := &Coordinator{
		cfg:           cfg,
		provider:      provider,
		messenger:     messenger,
		registry:      registry,
		store:         store,
		names:         make(map[string]*nameState),
		platformIndex: make(map[string]string),
		pendingHandle: make(map[string]string),
		unregistered:  make(map[string]providers.Event),
	}
	window := time.Duration(cfg.AnswerWindowSecs) * time.Second
	c.pipeline = challenges.NewPipeline(c, window)
	return c
}

// MeetingID returns the internal meeting ID of this session.
func (c *Coordinator) MeetingID() string { return c.cfg.MeetingID }

// Run drives the session event loop. It returns when the meeting ends, ctx is
// cancelled, or an unrecoverable error occurs.
//
// The Messenger is not started here — it runs for the whole daemon
// process so registrations work before any meeting. The caller routes
// messenger events into this coordinator via HandleMessengerEvent
// (typically through messengers.Router).
func (c *Coordinator) Run(ctx context.Context) error {
	providerEvents, err := c.provider.Subscribe(ctx, c.cfg.PlatformMeetingID)
	if err != nil {
		return fmt.Errorf("session: subscribe to provider: %w", err)
	}

	defer func() { //nolint:contextcheck // cleanup must run after ctx is cancelled or the provider closed; uses a fresh context
		c.pipeline.Drain()
		c.writeEvent(eventstore.Record{EventType: "meeting_ended"})
		if err := c.store.Close(context.Background()); err != nil {
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
		}
	}
}

// HandleMessengerEvent processes one event delivered by the Messenger.
// Safe to call concurrently with Run; the coordinator's state is
// guarded by an internal mutex.
func (c *Coordinator) HandleMessengerEvent(ctx context.Context, evt messengers.Event) {
	c.handleMessengerEvent(ctx, evt)
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

func (c *Coordinator) onMeetingStarted(_ context.Context, evt providers.Event) {
	c.store.SetStartTime(evt.Timestamp)
	c.writeEvent(eventstore.Record{
		Timestamp: evt.Timestamp,
		EventType: "meeting_started",
		Metadata:  map[string]string{"platform": c.provider.Name()},
	})
}

// onJoin runs the verification-gated state machine for a provider join event.
// Nothing is written to Parquet until verification arrives.
func (c *Coordinator) onJoin(ctx context.Context, evt providers.Event) {
	entry, registered := c.registry.Resolve(evt.DisplayName)
	if !registered || entry.MessengerName != c.messenger.Name() {
		// Either no registration at all, or the registration belongs to a
		// messenger we're not running. Either way, treat as unregistered.
		c.markUnregistered(evt)
		return
	}
	handle := entry.Handle

	key := normName(entry.DisplayName)

	c.mu.Lock()
	state := c.names[key]
	if state == nil {
		state = &nameState{
			canonicalName: entry.DisplayName,
			handle:        handle,
			platformIDs:   make(map[string]struct{}),
		}
		c.names[key] = state
	}
	state.platformIDs[evt.PlatformID] = struct{}{}
	c.platformIndex[evt.PlatformID] = key

	switch {
	case state.tainted:
		c.mu.Unlock()
		slog.Info("session: ignoring join on tainted name", "name", entry.DisplayName, "platform_id", evt.PlatformID)
		return

	case state.verifiedPlatformID != "":
		// Already-verified participant; a second platformID claiming the
		// same name is silently ignored (no DM, no Parquet).
		c.mu.Unlock()
		slog.Info("session: ignoring extra join under already-verified name", "name", entry.DisplayName, "platform_id", evt.PlatformID)
		return

	case state.pending != nil:
		// Pre-verification collision: drop the buffer, edit the DM,
		// taint the name. Nothing for this name will be processed until
		// every claimant has left the meeting.
		oldRef := state.confirmRef
		oldHandle := state.handle
		state.pending = nil
		state.confirmRef = messengers.MessageRef{}
		state.tainted = true
		delete(c.pendingHandle, oldHandle)
		c.mu.Unlock()

		_ = c.messenger.EditMessage(ctx, oldRef,
			fmt.Sprintf("⚠ Verification cancelled — another participant joined the meeting with the name %q. "+
				"Once everyone using this name leaves, you can re-join to verify.", entry.DisplayName))
		slog.Info("session: collision tainted name", "name", entry.DisplayName)
		return
	}

	// Empty slot: buffer the join and send the verification DM.
	state.pending = &bufferedJoin{
		platformID: evt.PlatformID,
		joinedAt:   evt.Timestamp,
		metadata:   evt.Extra,
	}
	c.pendingHandle[handle] = key
	c.mu.Unlock()

	ref, err := c.messenger.SendJoinConfirmation(ctx, handle, c.cfg.MeetingID, c.provider.Name())
	if err != nil {
		slog.Warn("session: send join confirmation", "err", err)
		c.mu.Lock()
		if state.pending != nil && state.pending.platformID == evt.PlatformID {
			state.pending = nil
		}
		delete(c.pendingHandle, handle)
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	if state.pending != nil && state.pending.platformID == evt.PlatformID {
		state.confirmRef = ref
	}
	c.mu.Unlock()
}

func (c *Coordinator) markUnregistered(evt providers.Event) {
	c.mu.Lock()
	c.unregistered[evt.PlatformID] = evt
	c.mu.Unlock()
	slog.Info("session: unregistered participant joined (live-only)", "name", evt.DisplayName)
}

func (c *Coordinator) onLeave(ctx context.Context, evt providers.Event) {
	c.mu.Lock()
	if _, ok := c.unregistered[evt.PlatformID]; ok {
		delete(c.unregistered, evt.PlatformID)
		c.mu.Unlock()
		return
	}

	key, ok := c.platformIndex[evt.PlatformID]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.platformIndex, evt.PlatformID)

	state := c.names[key]
	if state == nil {
		c.mu.Unlock()
		return
	}
	delete(state.platformIDs, evt.PlatformID)

	var (
		droppedPending    bool
		droppedConfirmRef messengers.MessageRef
		verifiedLeft      bool
		leftDisplayName   string
	)
	if state.pending != nil && state.pending.platformID == evt.PlatformID {
		droppedPending = true
		droppedConfirmRef = state.confirmRef
		delete(c.pendingHandle, state.handle)
		state.pending = nil
		state.confirmRef = messengers.MessageRef{}
	}
	if state.verifiedPlatformID == evt.PlatformID {
		verifiedLeft = true
		leftDisplayName = state.canonicalName
		state.verifiedPlatformID = ""
	}
	if len(state.platformIDs) == 0 {
		delete(c.names, key)
	}
	c.mu.Unlock()

	if droppedPending && droppedConfirmRef.Opaque != "" {
		_ = c.messenger.EditMessage(ctx, droppedConfirmRef,
			"Verification cancelled — you left the meeting before confirming.")
	}
	if verifiedLeft {
		c.writeEvent(eventstore.Record{
			EventType:   "participant_left",
			DisplayName: leftDisplayName,
		})
	}
}

// Status returns a snapshot of the current session state.
func (c *Coordinator) Status() CoordStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	present := make([]PresenceStatus, 0, len(c.names))
	for _, state := range c.names {
		if state.verifiedPlatformID == "" {
			continue
		}
		present = append(present, PresenceStatus{
			DisplayName: state.canonicalName,
			PlatformID:  state.verifiedPlatformID,
			JoinedAt:    state.verifiedAt,
		})
	}

	unreg := make([]UnregisteredStatus, 0, len(c.unregistered))
	for _, evt := range c.unregistered {
		unreg = append(unreg, UnregisteredStatus{
			DisplayName: evt.DisplayName,
			PlatformID:  evt.PlatformID,
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

// onJoinConfirmation handles a Yes/No tap on the verification DM.
// Nothing about the participant has reached Parquet before this point — on
// Yes we flush the buffered join with its original timestamp; on No the
// buffer is dropped silently.
func (c *Coordinator) onJoinConfirmation(_ context.Context, evt messengers.Event) {
	c.mu.Lock()
	key, ok := c.pendingHandle[evt.Handle]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.pendingHandle, evt.Handle)

	state := c.names[key]
	if state == nil || state.pending == nil {
		c.mu.Unlock()
		return
	}
	pending := state.pending
	state.pending = nil
	state.confirmRef = messengers.MessageRef{}

	if !evt.Confirmed {
		c.mu.Unlock()
		slog.Info("session: participant denied verification", "name", state.canonicalName)
		return
	}

	state.verifiedPlatformID = pending.platformID
	state.verifiedAt = evt.Timestamp
	canonicalName := state.canonicalName
	c.mu.Unlock()

	c.writeEvent(eventstore.Record{
		Timestamp:   pending.joinedAt,
		EventType:   "participant_joined",
		DisplayName: canonicalName,
		Metadata:    pending.metadata,
	})
	slog.Info("session: participant verified", "name", canonicalName)
}

// onRegistration handles a /register event. If anyone in the live-only
// unregistered list matches the newly registered display name, replay
// their original join through onJoin — the regular state machine takes
// it from there (including the collision rule when more than one such
// participant is currently in the meeting).
func (c *Coordinator) onRegistration(ctx context.Context, evt messengers.Event) {
	normIncoming := normName(evt.DisplayName)

	c.mu.Lock()
	matches := make([]providers.Event, 0)
	for pid, joinEvt := range c.unregistered {
		if normName(joinEvt.DisplayName) == normIncoming {
			matches = append(matches, joinEvt)
			delete(c.unregistered, pid)
		}
	}
	c.mu.Unlock()

	for _, joinEvt := range matches {
		c.onJoin(ctx, joinEvt)
	}
}

// RunPoll loads a question bank from disk and dispatches one poll round
// to the eligible participants currently in the meeting. autoSubmitted
// is stamped onto every challenge_issued event for this round; it is
// true only when the in-process challenger dispatched the bank without
// teacher review.
func (c *Coordinator) RunPoll(ctx context.Context, bankPath string, autoSubmitted bool) (challenges.PollResult, error) {
	bank, err := challenges.Load(bankPath)
	if err != nil {
		return challenges.PollResult{}, err
	}
	return c.runPollBank(ctx, bank, autoSubmitted)
}

// RunPollBank dispatches an in-memory bank through the same pipeline as
// RunPoll. Used by the in-process auto-generator on the auto_submit
// path so the generated bank never touches disk.
func (c *Coordinator) RunPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error) {
	return c.runPollBank(ctx, bank, autoSubmitted)
}

func (c *Coordinator) runPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error) {
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
	return c.pipeline.RunPoll(ctx, bank, autoSubmitted, eligible, sendFn, c.cfg.QuestionsDir, c.cfg.MeetingID)
}

// RecordGeneratorFailed records a challenger generator failure so the
// teacher sees it in the GUI's system log. Implements
// challenger.EventSink.
func (c *Coordinator) RecordGeneratorFailed(_ context.Context, reason string) {
	c.writeEvent(eventstore.Record{
		EventType: "challenge_generator_failed",
		Metadata:  map[string]string{"reason": reason},
	})
}

func (c *Coordinator) eligibleParticipants() []challenges.EligibleParticipant {
	minGap := time.Duration(c.cfg.MinGapBetweenChallengesSecs) * time.Second
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var out []challenges.EligibleParticipant
	for _, state := range c.names {
		if state.verifiedPlatformID == "" || state.tainted {
			continue
		}
		if !state.lastChallenge.IsZero() && now.Sub(state.lastChallenge) < minGap {
			continue
		}
		out = append(out, challenges.EligibleParticipant{
			DisplayName: state.canonicalName,
			Handle:      state.handle,
		})
	}
	return out
}

func (c *Coordinator) writeEvent(r eventstore.Record) {
	r.MeetingID = c.cfg.MeetingID
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	c.store.Append(r)
}

func (c *Coordinator) RecordChallengeIssued(_ context.Context, issued challenges.IssuedChallenge) error {
	c.mu.Lock()
	if state, ok := c.names[normName(issued.DisplayName)]; ok {
		state.lastChallenge = time.Now()
	}
	c.mu.Unlock()

	c.writeEvent(eventstore.Record{
		EventType:   "challenge_issued",
		DisplayName: issued.DisplayName,
		ChallengeID: issued.ChallengeID,
		QuestionID:  issued.Question.QuestionID,
		Metadata: map[string]string{
			"auto_submitted":  strconv.FormatBool(issued.AutoSubmitted),
			"answer_window_s": strconv.Itoa(c.cfg.AnswerWindowSecs),
		},
	})
	return nil
}

func (c *Coordinator) RecordChallengeResult(_ context.Context, challengeID string, result challenges.ScoreResult, latencyMS int64) error {
	evtType := "challenge_answered_correct"
	if result == challenges.ScoreIncorrect {
		evtType = "challenge_answered_incorrect"
	}
	c.writeEvent(eventstore.Record{
		EventType:   evtType,
		ChallengeID: challengeID,
		Metadata: map[string]string{
			"latency_ms": strconv.FormatInt(latencyMS, 10),
		},
	})
	return nil
}

func (c *Coordinator) RecordChallengeUnanswered(_ context.Context, challengeID string) error {
	c.writeEvent(eventstore.Record{
		EventType:   "challenge_unanswered",
		ChallengeID: challengeID,
	})
	return nil
}

// RecordChallengeSkipped is a no-op: the eligible list is filtered to verified
// participants before dispatch, so no skip reason produced by the pipeline
// today warrants a Parquet record. Kept to satisfy challenges.EventSink.
func (c *Coordinator) RecordChallengeSkipped(_ context.Context, _, _ string) error {
	return nil
}

func (c *Coordinator) DeleteMessage(ctx context.Context, ref string) error {
	return c.messenger.DeleteMessage(ctx, messengers.MessageRef{Opaque: ref})
}

// normName matches participants.normName: case-sensitive, trims surrounding
// whitespace only. The two must stay in sync — registry keys flow through
// here as session map keys.
func normName(displayName string) string {
	return strings.TrimSpace(displayName)
}
