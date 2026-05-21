package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
)

// pendingKey identifies a question message whose reply is expected as an answer.
type pendingKey struct {
	chatID    int64
	messageID int
}

// Adapter implements messengers.Messenger using the Telegram Bot API.
type Adapter struct {
	bot       *tgbotapi.BotAPI
	registry  participants.Registry
	platforms []string // enabled platform names for /register help text
	events    chan messengers.Event

	mu         sync.Mutex
	pending    map[pendingKey]string // {chatID, questionMsgID} → challengeID
	pendingInv map[string]pendingKey // challengeID → pendingKey (for cleanup on timeout)
}

// New creates a Telegram adapter.
// registry is called directly by /register command handlers.
// platforms lists enabled platform names shown in the help message.
func New(token string, registry participants.Registry, platforms []string) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	return &Adapter{
		bot:        bot,
		registry:   registry,
		platforms:  platforms,
		events:     make(chan messengers.Event, 64),
		pending:    make(map[pendingKey]string),
		pendingInv: make(map[string]pendingKey),
	}, nil
}

func (a *Adapter) Name() string { return "telegram" }

// Start begins polling the Telegram API for updates. The returned channel is
// closed when ctx is cancelled.
func (a *Adapter) Start(ctx context.Context) (<-chan messengers.Event, error) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := a.bot.GetUpdatesChan(u)

	go func() {
		defer close(a.events)
		for {
			select {
			case <-ctx.Done():
				a.bot.StopReceivingUpdates()
				return
			case upd, ok := <-updates:
				if !ok {
					return
				}
				a.handleUpdate(ctx, upd)
			}
		}
	}()

	return a.events, nil
}

// Stop gracefully shuts down the Telegram poller.
func (a *Adapter) Stop(_ context.Context) error {
	a.bot.StopReceivingUpdates()
	return nil
}

func (a *Adapter) handleUpdate(ctx context.Context, upd tgbotapi.Update) {
	switch {
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "start":
		a.handleStart(upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "register":
		a.handleRegister(ctx, upd.Message)
	case upd.Message != nil && upd.Message.Text != "":
		a.handleTextMessage(upd.Message)
	case upd.CallbackQuery != nil:
		a.handleCallback(upd.CallbackQuery)
	}
}

func (a *Adapter) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	// TODO: use the configured UI language for this message
	text := fmt.Sprintf(
		"Welcome to Presence Tracker!\n\nTo receive attendance challenges, register your display name:\n\n"+
			"/register <platform> <your display name>\n\n"+
			"Example: /register zoom John Smith\n\n"+
			"Platforms: %s, all\n\n"+
			"Use /register all <name> to register for all platforms at once.",
		strings.Join(a.platforms, ", "),
	)
	reply := tgbotapi.NewMessage(chatID, text)
	_, _ = a.bot.Send(reply)
}

func (a *Adapter) handleRegister(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)
	label := userLabel(msg.From)

	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		// TODO: use configured UI language
		text := fmt.Sprintf(
			"Usage: /register <platform> <your display name>\n\nExample: /register zoom John Smith\n\nPlatforms: %s, all",
			strings.Join(a.platforms, ", "),
		)
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, text))
		return
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, "Please provide both a platform and your display name.\n\nUsage: /register <platform> <display name>"))
		return
	}

	platform := strings.ToLower(parts[0])
	displayName := strings.TrimSpace(parts[1])

	var platformsToRegister []string
	if platform == "all" {
		platformsToRegister = a.platforms
	} else {
		valid := slices.Contains(a.platforms, platform)
		if !valid {
			_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf(
				"Unknown platform %q. Available: %s, all",
				platform, strings.Join(a.platforms, ", "),
			)))
			return
		}
		platformsToRegister = []string{platform}
	}

	var successes, failures []string
	for _, p := range platformsToRegister {
		_, err := a.registry.Register(ctx, "telegram", participants.Handle(handle), label, p, displayName)
		if errors.Is(err, participants.ErrNameTaken) {
			failures = append(failures, fmt.Sprintf("%s: name already registered by another account — ask your teacher to remove it via the registry page", p))
		} else if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", p, err.Error()))
		} else {
			successes = append(successes, p)
		}
	}

	var lines []string
	if len(successes) > 0 {
		// TODO: use configured UI language
		lines = append(lines, fmt.Sprintf("✓ Registered \"%s\" on: %s\n\nWhen you join a meeting, you'll receive a confirmation message.", displayName, strings.Join(successes, ", ")))
	}
	if len(failures) > 0 {
		lines = append(lines, "⚠ Could not register:\n"+strings.Join(failures, "\n"))
	}

	reply := tgbotapi.NewMessage(chatID, strings.Join(lines, "\n\n"))
	_, _ = a.bot.Send(reply)

	for _, p := range successes {
		a.events <- messengers.Event{
			Kind:        messengers.EventKindRegistration,
			Handle:      handle,
			Platform:    p,
			DisplayName: displayName,
			Timestamp:   time.Now().UTC(),
		}
	}
}

func (a *Adapter) handleTextMessage(msg *tgbotapi.Message) {
	// Only replies to a question message are accepted as answers.
	if msg.ReplyToMessage == nil {
		return
	}

	key := pendingKey{chatID: msg.Chat.ID, messageID: msg.ReplyToMessage.MessageID}

	a.mu.Lock()
	cid, hasPending := a.pending[key]
	if hasPending {
		delete(a.pending, key)
		delete(a.pendingInv, cid)
	}
	a.mu.Unlock()

	if !hasPending {
		return
	}

	handle := strconv.FormatInt(msg.Chat.ID, 10)
	ansRef := telegramRef{ChatID: msg.Chat.ID, MessageID: msg.MessageID}
	ansRefJSON, _ := json.Marshal(ansRef) //nolint:errchkjson // telegramRef is a plain int64 struct; Marshal cannot fail

	a.events <- messengers.Event{
		Kind:             messengers.EventKindAnswerReceived,
		Handle:           handle,
		ChallengeID:      cid,
		Answer:           msg.Text,
		AnswerMessageRef: messengers.MessageRef{Opaque: string(ansRefJSON)},
		Timestamp:        time.Now().UTC(),
	}
}

func (a *Adapter) handleCallback(cq *tgbotapi.CallbackQuery) {
	if strings.HasPrefix(cq.Data, "join:") {
		a.handleConfirmationCallback(cq)
		return
	}
	a.handleChallengeCallback(cq)
}

func (a *Adapter) handleChallengeCallback(cq *tgbotapi.CallbackQuery) {
	// Callback data format: "<challengeID>:<choiceLabel>"
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 {
		return
	}
	cid, choice := parts[0], parts[1]
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	a.events <- messengers.Event{
		Kind:        messengers.EventKindAnswerReceived,
		Handle:      handle,
		ChallengeID: cid,
		Answer:      choice,
		Selected:    []string{choice},
		Timestamp:   time.Now().UTC(),
	}
}

func (a *Adapter) handleConfirmationCallback(cq *tgbotapi.CallbackQuery) {
	confirmed := cq.Data == "join:yes"
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	var confirmRef messengers.MessageRef
	if cq.Message != nil {
		// TODO: use configured UI language
		var responseText string
		if confirmed {
			responseText = "✓ Confirmed! Challenges will be sent to you during this meeting."
		} else {
			responseText = "Not confirmed. Challenges will not be sent to you in this session."
		}
		edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, responseText)
		empty := tgbotapi.NewInlineKeyboardMarkup()
		edit.ReplyMarkup = &empty
		_, _ = a.bot.Send(edit)

		r := telegramRef{ChatID: chatID, MessageID: cq.Message.MessageID}
		b, _ := json.Marshal(r) //nolint:errchkjson // telegramRef is a plain int64 struct; Marshal cannot fail
		confirmRef = messengers.MessageRef{Opaque: string(b)}
	}

	a.events <- messengers.Event{
		Kind:            messengers.EventKindJoinConfirmation,
		Handle:          handle,
		Confirmed:       confirmed,
		ConfirmationRef: confirmRef,
		Timestamp:       time.Now().UTC(),
	}
}

// SendJoinConfirmation sends a "Did you just join?" DM with Yes/No buttons.
func (a *Adapter) SendJoinConfirmation(_ context.Context, handle, meetingID, platform string) (messengers.MessageRef, error) {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}

	// TODO: use configured UI language
	text := fmt.Sprintf("📍 Did you just join meeting *%s* on *%s*?", meetingID, platform)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		[]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Yes, that's me", "join:yes"),
			tgbotapi.NewInlineKeyboardButtonData("No", "join:no"),
		},
	)

	sent, err := a.bot.Send(msg)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: send join confirmation: %w", err)
	}

	ref := telegramRef{ChatID: chatID, MessageID: sent.MessageID}
	b, _ := json.Marshal(ref) //nolint:errchkjson // telegramRef is a plain int64 struct; Marshal cannot fail
	return messengers.MessageRef{Opaque: string(b)}, nil
}

// SendChallenge sends a challenge prompt to a student.
func (a *Adapter) SendChallenge(ctx context.Context, handle string, c messengers.ChallengePrompt) (messengers.MessageRef, error) {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}

	var msg tgbotapi.MessageConfig
	if c.QuestionType == string(challenges.MultipleChoice) && len(c.Choices) > 0 {
		msg = buildMCQMessage(chatID, c)
	} else {
		msg = buildTextMessage(chatID, c)
	}

	sent, err := a.bot.Send(msg)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: send challenge: %w", err)
	}

	if c.QuestionType != string(challenges.MultipleChoice) {
		key := pendingKey{chatID: chatID, messageID: sent.MessageID}
		a.mu.Lock()
		a.pending[key] = c.ChallengeID
		a.pendingInv[c.ChallengeID] = key
		a.mu.Unlock()
	}

	ref := telegramRef{ChatID: chatID, MessageID: sent.MessageID}
	b, _ := json.Marshal(ref) //nolint:errchkjson // telegramRef is a plain int64 struct; Marshal cannot fail
	return messengers.MessageRef{Opaque: string(b)}, nil
}

func buildMCQMessage(chatID int64, c messengers.ChallengePrompt) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, c.Prompt)
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, choice := range c.Choices {
		data := c.ChallengeID + ":" + choice
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(choice, data),
		})
	}
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	return msg
}

func buildTextMessage(chatID int64, c messengers.ChallengePrompt) tgbotapi.MessageConfig {
	// TODO: use the configured UI language for these prompts
	suffix := "\n\nPlease reply to this message with your answer."
	if c.QuestionType == string(challenges.Numeric) {
		suffix = "\n\nPlease reply to this message with a number."
	}
	return tgbotapi.NewMessage(chatID, c.Prompt+suffix)
}

// EditMessage replaces the text of a previously sent message.
func (a *Adapter) EditMessage(_ context.Context, ref messengers.MessageRef, newText string) error {
	var r telegramRef
	if err := json.Unmarshal([]byte(ref.Opaque), &r); err != nil {
		return fmt.Errorf("telegram: decode ref: %w", err)
	}
	edit := tgbotapi.NewEditMessageText(r.ChatID, r.MessageID, newText)
	empty := tgbotapi.NewInlineKeyboardMarkup()
	edit.ReplyMarkup = &empty
	_, err := a.bot.Send(edit)
	return err
}

// DeleteMessage deletes a previously sent message.
func (a *Adapter) DeleteMessage(_ context.Context, ref messengers.MessageRef) error {
	var r telegramRef
	if err := json.Unmarshal([]byte(ref.Opaque), &r); err != nil {
		return fmt.Errorf("telegram: decode ref: %w", err)
	}
	del := tgbotapi.NewDeleteMessage(r.ChatID, r.MessageID)
	_, err := a.bot.Request(del)
	return err
}

type telegramRef struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
}

// userLabel builds a human-readable Telegram label for the registry.
func userLabel(u *tgbotapi.User) string {
	if u == nil {
		return ""
	}
	if u.UserName != "" {
		return "@" + u.UserName
	}
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	return name
}
