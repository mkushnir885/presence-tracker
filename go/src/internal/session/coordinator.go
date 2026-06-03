package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
)

const (
	joinConfirmationTTL = 2 * time.Minute
	providerRefreshWait = 5 * time.Second
)

var _ challenges.EventSink = (*Coordinator)(nil)

// bufferedJoin is a participant_joined held in memory until the student
// confirms; nothing reaches Parquet before verification.
type bufferedJoin struct {
	platformID string
	joinedAt   time.Time
	metadata   map[string]string
}

// nameState tracks one display name through the join-verification flow
// (keyed by normalized name). Created when a registered name first joins,
// destroyed once no platformID claims it.
type nameState struct {
	canonicalName      string              // as registered, written verbatim to Parquet
	handle             string              // messenger handle for the verification DM
	language           string              // recipient catalog language
	platformIDs        map[string]struct{} // every current claimant of this name
	pending            *bufferedJoin       // join awaiting Yes/No, if any
	confirmRef         messengers.MessageRef
	confirmTimer       *time.Timer // expiry for pending; nil when not pending
	verifiedPlatformID string      // the verified claimant; "" if none
	verifiedAt         time.Time
	lastChallenge      time.Time // for the min-gap-between-challenges rule
	tainted            bool      // pre-verification collision; ignore until the name clears
}

type CoordStatus struct {
	MeetingID         string
	Present           []PresenceStatus
	Unregistered      []UnregisteredStatus
	MeetingStartedAt  time.Time
	MeetingInProgress bool
}

type PresenceStatus struct {
	DisplayName string
	PlatformID  string
	JoinedAt    time.Time
}

type UnregisteredStatus struct {
	DisplayName string
	PlatformID  string
}

type Config struct {
	MeetingID         string
	PlatformMeetingID string
	ProviderName      string
}

type Coordinator struct {
	cfg       Config
	dyn       *config.Config
	provider  providers.Provider
	messenger messengers.Messenger
	registry  participants.Registry
	store     *eventstore.Writer
	pipeline  *challenges.Pipeline

	mu            sync.Mutex
	names         map[string]*nameState      // normalized name → state
	platformIndex map[string]string          // platformID → normalized name
	pendingHandle map[string]string          // messenger handle → normalized name (while pending)
	unregistered  map[string]providers.Event // platformID → join event (live GUI only, never persisted)

	meetingEndedAt time.Time // set when the provider reports the meeting ended

	meetingStartedAt  time.Time // set on the first session_started observation
	meetingInProgress bool
}

func New(cfg Config, dyn *config.Config, provider providers.Provider, messenger messengers.Messenger, registry participants.Registry, store *eventstore.Writer) *Coordinator {
	c := &Coordinator{
		cfg:           cfg,
		dyn:           dyn,
		provider:      provider,
		messenger:     messenger,
		registry:      registry,
		store:         store,
		names:         make(map[string]*nameState),
		platformIndex: make(map[string]string),
		pendingHandle: make(map[string]string),
		unregistered:  make(map[string]providers.Event),
	}
	c.pipeline = challenges.NewPipeline(c)
	return c
}

func (c *Coordinator) Run(ctx context.Context) error {
	providerEvents, err := c.provider.Subscribe(ctx, c.cfg.PlatformMeetingID)
	if err != nil {
		return fmt.Errorf("session: subscribe to provider: %w", err)
	}

	defer func() { //nolint:contextcheck // cleanup must run after ctx is cancelled or the provider closed; uses a fresh context
		c.pipeline.Drain()
		c.cancelAllConfirmTimers()
		c.writeSessionEnded()
		if _, err := c.store.Close(context.Background()); err != nil {
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

func (c *Coordinator) handleProviderEvent(ctx context.Context, evt providers.Event) {
	switch evt.Kind {
	case providers.EventKindParticipantJoined:
		c.onJoin(ctx, evt)
	case providers.EventKindParticipantLeft:
		c.onLeave(ctx, evt)
	case providers.EventKindMeetingEnded:
		c.onMeetingEnded(evt)
	case providers.EventKindMeetingStarted:
		c.onMeetingStarted(evt)
	}
}

func (c *Coordinator) onMeetingStarted(evt providers.Event) {
	cause := causeMeeting
	if evt.MeetingInProgress {
		cause = causeTracking
	}
	c.mu.Lock()
	c.meetingStartedAt = evt.Timestamp
	c.meetingInProgress = evt.MeetingInProgress
	c.mu.Unlock()
	c.store.SetStartTime(evt.Timestamp)
	c.writeEventAt(evt.Timestamp, eventstore.Record{
		EventType: "session_started",
		Metadata: map[string]string{
			"platform":     c.provider.Name(),
			"cause":        cause,
			"timestamp_ms": strconv.FormatInt(evt.Timestamp.UnixMilli(), 10),
		},
	})
}

func (c *Coordinator) onMeetingEnded(evt providers.Event) {
	c.mu.Lock()
	c.meetingEndedAt = evt.Timestamp
	c.mu.Unlock()
}

const (
	causeMeeting  = "meeting"
	causeTracking = "tracking"
)

func (c *Coordinator) writeSessionEnded() {
	c.mu.Lock()
	startedAt := c.meetingStartedAt
	endedAt := c.meetingEndedAt
	c.mu.Unlock()

	// No session_started observed means the writer buffer is empty, so
	// Close leaves no Parquet file: tracking sessions that never saw a
	// meeting produce no phantom file.
	if startedAt.IsZero() {
		slog.Info("session: no meeting observed — skipping session_ended and Parquet file")
		return
	}

	cause := causeTracking
	ts := time.Now().UTC()
	if !endedAt.IsZero() {
		cause = causeMeeting
		ts = endedAt
	}
	c.writeEventAt(ts, eventstore.Record{
		EventType: "session_ended",
		Metadata: map[string]string{
			"cause":        cause,
			"timestamp_ms": strconv.FormatInt(ts.UnixMilli(), 10),
		},
	})
}

func (c *Coordinator) onJoin(ctx context.Context, evt providers.Event) {
	entry, registered := c.registry.Resolve(evt.DisplayName)
	if !registered || entry.MessengerName != c.messenger.Name() {
		c.markUnregistered(evt)
		return
	}
	handle := entry.Handle

	key := participants.NormalizeName(entry.DisplayName)

	c.mu.Lock()
	state := c.names[key]
	if state == nil {
		state = &nameState{
			canonicalName: entry.DisplayName,
			handle:        handle,
			language:      entry.Language,
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
		c.mu.Unlock()
		slog.Info("session: ignoring extra join under already-verified name", "name", entry.DisplayName, "platform_id", evt.PlatformID)
		return

	// A second claimant arriving before the first verifies taints the
	// name: drop the buffer, cancel the in-flight DM, and ignore every
	// further join under it until all claimants have left.
	case state.pending != nil:
		oldRef := state.confirmRef
		oldHandle := state.handle
		state.pending = nil
		state.confirmRef = messengers.MessageRef{}
		state.tainted = true
		c.cancelConfirmTimerLocked(state)
		delete(c.pendingHandle, oldHandle)
		c.mu.Unlock()

		_ = c.messenger.Notify(ctx, oldRef, entry.Language, messengers.NotifyJoinCollision, entry.DisplayName)
		slog.Info("session: collision tainted name", "name", entry.DisplayName)
		return
	}

	state.pending = &bufferedJoin{
		platformID: evt.PlatformID,
		joinedAt:   evt.Timestamp,
		metadata:   evt.Extra,
	}
	c.pendingHandle[handle] = key
	c.mu.Unlock()

	ref, err := c.messenger.SendJoinConfirmation(ctx, handle, entry.Language, c.cfg.PlatformMeetingID, c.provider.DisplayName())
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
		expected := state.pending
		state.confirmTimer = time.AfterFunc(joinConfirmationTTL, func() { //nolint:contextcheck // timer fires detached from request ctx; uses background
			c.expireJoinConfirmation(context.Background(), key, expected)
		})
	}
	c.mu.Unlock()
}

func (c *Coordinator) cancelConfirmTimerLocked(state *nameState) {
	if state.confirmTimer != nil {
		state.confirmTimer.Stop()
		state.confirmTimer = nil
	}
}

func (c *Coordinator) cancelAllConfirmTimers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.names {
		c.cancelConfirmTimerLocked(state)
	}
}

// expireJoinConfirmation drops a verification buffer whose window elapsed.
// The expected pointer guards against a late timer firing on a buffer that
// a newer join already replaced under the same name.
func (c *Coordinator) expireJoinConfirmation(ctx context.Context, key string, expected *bufferedJoin) {
	c.mu.Lock()
	state := c.names[key]
	if state == nil || state.pending != expected {
		c.mu.Unlock()
		return
	}
	ref := state.confirmRef
	handle := state.handle
	lang := state.language
	state.pending = nil
	state.confirmRef = messengers.MessageRef{}
	state.confirmTimer = nil
	delete(c.pendingHandle, handle)
	if len(state.platformIDs) == 0 {
		delete(c.names, key)
	}
	c.mu.Unlock()

	if ref.Opaque != "" {
		_ = c.messenger.Notify(ctx, ref, lang, messengers.NotifyJoinTimedOut)
	}
	slog.Info("session: verification timed out", "name", state.canonicalName)
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
		droppedLang       string
		verifiedLeft      bool
		leftDisplayName   string
	)
	if state.pending != nil && state.pending.platformID == evt.PlatformID {
		droppedPending = true
		droppedConfirmRef = state.confirmRef
		droppedLang = state.language
		c.cancelConfirmTimerLocked(state)
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
	meetingEnded := !c.meetingEndedAt.IsZero()
	c.mu.Unlock()

	if droppedPending && droppedConfirmRef.Opaque != "" {
		_ = c.messenger.Notify(ctx, droppedConfirmRef, droppedLang, messengers.NotifyJoinDropped)
	}
	if verifiedLeft && !meetingEnded {
		c.writeEvent(eventstore.Record{
			EventType:   "participant_left",
			DisplayName: leftDisplayName,
		})
	}
}

func (c *Coordinator) Status() CoordStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	present := make([]PresenceStatus, 0, len(c.names))
	unreg := make([]UnregisteredStatus, 0, len(c.unregistered)+len(c.names))
	for _, evt := range c.unregistered {
		unreg = append(unreg, UnregisteredStatus{
			DisplayName: evt.DisplayName,
			PlatformID:  evt.PlatformID,
		})
	}
	for _, state := range c.names {
		if state.verifiedPlatformID != "" {
			present = append(present, PresenceStatus{
				DisplayName: state.canonicalName,
				PlatformID:  state.verifiedPlatformID,
				JoinedAt:    state.verifiedAt,
			})
		}
		for pid := range state.platformIDs {
			if pid == state.verifiedPlatformID {
				continue
			}
			unreg = append(unreg, UnregisteredStatus{
				DisplayName: state.canonicalName,
				PlatformID:  pid,
			})
		}
	}

	return CoordStatus{
		MeetingID:         c.cfg.MeetingID,
		Present:           present,
		Unregistered:      unreg,
		MeetingStartedAt:  c.meetingStartedAt,
		MeetingInProgress: c.meetingInProgress,
	}
}

func (c *Coordinator) HandleMessengerEvent(ctx context.Context, evt messengers.Event) {
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
		if !c.pipeline.HandleAnswer(evt.Handle, answer) {
			slog.Debug("session: answer arrived after window closed", "challenge", evt.ChallengeID)
		}
	}
}

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
	c.cancelConfirmTimerLocked(state)

	if !evt.Confirmed {
		c.mu.Unlock()
		slog.Info("session: participant denied verification", "name", state.canonicalName)
		return
	}

	state.verifiedPlatformID = pending.platformID
	state.verifiedAt = evt.Timestamp
	canonicalName := state.canonicalName
	meetingStartedAt := c.meetingStartedAt
	c.mu.Unlock()

	joinedAt := pending.joinedAt
	if !meetingStartedAt.IsZero() && joinedAt.Before(meetingStartedAt) {
		joinedAt = meetingStartedAt
	}

	c.writeEventAt(joinedAt, eventstore.Record{
		EventType:   "participant_joined",
		DisplayName: canonicalName,
		Metadata:    pending.metadata,
	})
	slog.Info("session: participant verified", "name", canonicalName)
}

func (c *Coordinator) onRegistration(ctx context.Context, evt messengers.Event) {
	normIncoming := participants.NormalizeName(evt.DisplayName)

	c.mu.Lock()
	matches := make([]providers.Event, 0)
	for pid, joinEvt := range c.unregistered {
		if participants.NormalizeName(joinEvt.DisplayName) == normIncoming {
			matches = append(matches, joinEvt)
			delete(c.unregistered, pid)
		}
	}
	c.mu.Unlock()

	for _, joinEvt := range matches {
		c.onJoin(ctx, joinEvt)
	}
}

func (c *Coordinator) RunPollBank(ctx context.Context, bank challenges.Bank, autoSubmitted bool) (challenges.PollResult, error) {
	for i := range bank.Questions {
		bank.Questions[i].AutoSubmitted = autoSubmitted
	}
	challengeID := uuid.Must(uuid.NewV7()).String()

	if r, ok := c.provider.(providers.Refreshable); ok {
		r.Refresh()
		select {
		case <-time.After(providerRefreshWait):
		case <-ctx.Done():
			return challenges.PollResult{}, ctx.Err()
		}
	}
	eligible, ineligible := c.eligibleParticipants()
	for _, sk := range ineligible {
		_ = c.RecordChallengeSkipped(ctx, challenges.SkippedChallenge{
			ChallengeID:   challengeID,
			DisplayName:   sk.DisplayName,
			Reason:        sk.Reason,
			AutoSubmitted: autoSubmitted,
			SkippedAt:     time.Now().UTC(),
		})
	}

	sendFn := func(ctx context.Context, handle, lang, cid string, q challenges.Question) (string, error) {
		mp := messengers.ChallengePrompt{
			ChallengeID:  cid,
			QuestionID:   q.QuestionID,
			Prompt:       q.Prompt,
			QuestionType: string(q.QuestionType),
			Choices:      q.Choices,
		}
		ref, err := c.messenger.SendChallenge(ctx, handle, lang, mp)
		if err != nil {
			return "", err
		}
		return ref.Opaque, nil
	}
	answerWindow := time.Duration(c.dyn.Get().Challenges.Defaults.AnswerWindowSeconds) * time.Second
	return c.pipeline.RunPoll(ctx, bank, challengeID, answerWindow, eligible, sendFn, c.store.Dir())
}

func (c *Coordinator) RecordGeneratorFailed(_ context.Context, reason string) {
	c.writeEvent(eventstore.Record{
		EventType: "challenge_generator_failed",
		Metadata:  map[string]string{"reason": reason},
	})
}

type skippedParticipant struct {
	DisplayName string
	Reason      string
}

func (c *Coordinator) eligibleParticipants() ([]challenges.EligibleParticipant, []skippedParticipant) {
	minGap := time.Duration(c.dyn.Get().Challenges.Defaults.MinGapBetweenChallengesSecs) * time.Second
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var eligible []challenges.EligibleParticipant
	var skipped []skippedParticipant
	for _, state := range c.names {
		if state.verifiedPlatformID == "" || state.tainted {
			continue
		}
		if !state.lastChallenge.IsZero() && now.Sub(state.lastChallenge) < minGap {
			skipped = append(skipped, skippedParticipant{
				DisplayName: state.canonicalName,
				Reason:      "min_gap",
			})
			continue
		}
		eligible = append(eligible, challenges.EligibleParticipant{
			DisplayName: state.canonicalName,
			Handle:      state.handle,
			Language:    state.language,
		})
	}
	return eligible, skipped
}

func (c *Coordinator) writeEvent(r eventstore.Record) {
	c.writeEventAt(time.Now().UTC(), r)
}

// writeEventAt stamps r with the meeting ID and its from_start_ms offset
// (derived from at and the meeting start) before buffering it for the log.
func (c *Coordinator) writeEventAt(at time.Time, r eventstore.Record) {
	r.MeetingID = c.cfg.MeetingID
	r.FromStartMS = c.offsetMS(at)
	c.store.Append(r)
}

// offsetMS converts an absolute instant to ms since the meeting start. Events
// at or before the start (including session_started itself) yield 0.
func (c *Coordinator) offsetMS(at time.Time) int64 {
	c.mu.Lock()
	start := c.meetingStartedAt
	c.mu.Unlock()
	if start.IsZero() || at.Before(start) {
		return 0
	}
	return at.Sub(start).Milliseconds()
}

func (c *Coordinator) RecordChallengeIssued(_ context.Context, issued challenges.IssuedChallenge) error {
	c.mu.Lock()
	if state, ok := c.names[participants.NormalizeName(issued.DisplayName)]; ok {
		state.lastChallenge = time.Now()
	}
	c.mu.Unlock()

	c.writeEvent(eventstore.Record{
		EventType:   "challenge_issued",
		DisplayName: issued.DisplayName,
		ChallengeID: issued.ChallengeID,
		QuestionID:  issued.Question.QuestionID,
		Metadata: map[string]string{
			"auto_submitted":  strconv.FormatBool(issued.Question.AutoSubmitted),
			"answer_window_s": strconv.Itoa(c.dyn.Get().Challenges.Defaults.AnswerWindowSeconds),
		},
	})
	return nil
}

func (c *Coordinator) RecordChallengeResult(_ context.Context, challengeID string, result challenges.ScoreResult, submitted challenges.Answer, latencyMS int64) error {
	evtType := "challenge_answered_correct"
	if result == challenges.ScoreIncorrect {
		evtType = "challenge_answered_incorrect"
	}
	c.writeEvent(eventstore.Record{
		EventType:   evtType,
		ChallengeID: challengeID,
		Metadata: map[string]string{
			"latency_ms":       strconv.FormatInt(latencyMS, 10),
			"submitted_answer": encodeSubmittedAnswer(submitted),
		},
	})
	return nil
}

func encodeSubmittedAnswer(a challenges.Answer) string {
	if len(a.Selected) > 0 {
		buf, err := json.Marshal(a.Selected)
		if err != nil {
			return ""
		}
		return string(buf)
	}
	return a.Text
}

func (c *Coordinator) RecordChallengeUnanswered(_ context.Context, challengeID string) error {
	c.writeEvent(eventstore.Record{
		EventType:   "challenge_unanswered",
		ChallengeID: challengeID,
	})
	return nil
}

func (c *Coordinator) RecordChallengeSkipped(_ context.Context, sk challenges.SkippedChallenge) error {
	ts := sk.SkippedAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	c.writeEventAt(ts, eventstore.Record{
		EventType:   "challenge_skipped",
		ChallengeID: sk.ChallengeID,
		DisplayName: sk.DisplayName,
		Metadata: map[string]string{
			"reason":         sk.Reason,
			"auto_submitted": strconv.FormatBool(sk.AutoSubmitted),
		},
	})
	return nil
}

func (c *Coordinator) NotifyAnswered(ctx context.Context, handle, lang, questionRef, replyRef string) error {
	if err := c.messenger.DeleteMessage(ctx, messengers.MessageRef{Opaque: questionRef}); err != nil {
		slog.Debug("session: delete question message", "err", err)
	}
	if replyRef != "" {
		if err := c.messenger.DeleteMessage(ctx, messengers.MessageRef{Opaque: replyRef}); err != nil {
			slog.Debug("session: delete answer message", "err", err)
		}
	}
	return c.messenger.SendNotification(ctx, handle, lang, messengers.NotifyChallengeAnswered)
}

func (c *Coordinator) NotifyAnswerTimedOut(ctx context.Context, lang, ref string) error {
	return c.messenger.Notify(ctx, messengers.MessageRef{Opaque: ref}, lang, messengers.NotifyChallengeTimedOut)
}
