package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/i18n"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
)

// Name is the canonical identifier this adapter reports through
// Messenger.Name and registers under in the messengers catalog.
const Name = "telegram"

func init() {
	messengers.Register(Name)
}

// pendingKey identifies a question message whose reply is expected as an answer.
type pendingKey struct {
	chatID    int64
	messageID int
}

// Adapter implements messengers.Messenger using the Telegram Bot API.
type Adapter struct {
	bot      *tgbotapi.BotAPI
	registry participants.Registry
	events   chan messengers.Event
	catalog  *i18n.Catalog

	mu         sync.Mutex
	pending    map[pendingKey]string // {chatID, questionMsgID} → challengeID
	pendingInv map[string]pendingKey // challengeID → pendingKey (for cleanup on timeout)
	chatLang   map[int64]string      // chatID → catalog language, cached from incoming updates
}

// New creates a Telegram adapter.
// registry is called directly by /register command handlers.
func New(token string, registry participants.Registry) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	return &Adapter{
		bot:        bot,
		registry:   registry,
		events:     make(chan messengers.Event, 64),
		catalog:    newCatalog(),
		pending:    make(map[pendingKey]string),
		pendingInv: make(map[string]pendingKey),
		chatLang:   make(map[int64]string),
	}, nil
}

// rememberLang caches the language hint reported by an incoming
// Telegram user. Subsequent server-initiated sends (join confirmation,
// challenge prompts) use the cached value so the user keeps seeing the
// same language they have been writing in.
func (a *Adapter) rememberLang(chatID int64, code string) {
	lang := languageFromCode(code)
	a.mu.Lock()
	a.chatLang[chatID] = lang
	a.mu.Unlock()
}

// locale returns the Locale to use for chatID. Falls back to English
// when no incoming update from this chat has been seen yet —
// practically rare since users must /start before joining any meeting.
func (a *Adapter) locale(chatID int64) i18n.Locale {
	a.mu.Lock()
	lang, ok := a.chatLang[chatID]
	a.mu.Unlock()
	if !ok {
		lang = "en"
	}
	return a.catalog.Locale(lang)
}

func (a *Adapter) Name() string { return Name }

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
	if from := updateSender(upd); from != nil {
		a.rememberLang(senderChatID(upd), from.LanguageCode)
	}
	switch {
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "start":
		a.handleStart(upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "register":
		a.handleRegister(ctx, upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "unregister":
		a.handleUnregister(ctx, upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "whoami":
		a.handleWhoami(upd.Message.Chat.ID)
	case upd.Message != nil && upd.Message.Text != "":
		a.handleTextMessage(upd.Message)
	case upd.CallbackQuery != nil:
		a.handleCallback(upd.CallbackQuery)
	}
}

func (a *Adapter) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	locale := a.locale(chatID)
	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.start")))
}

func (a *Adapter) handleRegister(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)
	label := userLabel(msg.From)
	locale := a.locale(chatID)

	displayName := strings.TrimSpace(msg.CommandArguments())
	if displayName == "" {
		a.handleWhoami(chatID)
		return
	}

	err := a.registry.Register(ctx, a.Name(), handle, label, displayName)
	switch {
	case errors.Is(err, participants.ErrNameTaken):
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf(
			locale.T("messenger.register.name_taken"), displayName,
		)))
		return
	case err != nil:
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf(
			locale.T("messenger.register.failed"), err.Error(),
		)))
		return
	}

	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf(
		locale.T("messenger.register.success"), displayName,
	)))

	a.events <- messengers.Event{
		Kind:        messengers.EventKindRegistration,
		Handle:      handle,
		DisplayName: displayName,
		Timestamp:   time.Now().UTC(),
	}
}

func (a *Adapter) handleUnregister(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)
	locale := a.locale(chatID)

	n, err := a.registry.Delete(ctx, participants.Filter{MessengerName: a.Name(), Handle: handle})
	switch {
	case err != nil:
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf(
			locale.T("messenger.unregister.error"), err.Error(),
		)))
	case n == 0:
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, locale.T("messenger.whoami.none")))
	default:
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, locale.T("messenger.unregister.removed")))
	}
}

func (a *Adapter) handleWhoami(chatID int64) {
	handle := strconv.FormatInt(chatID, 10)
	locale := a.locale(chatID)
	entry, ok := a.registry.LookupByHandle(a.Name(), handle)
	if !ok {
		_, _ = a.bot.Send(tgbotapi.NewMessage(chatID,
			locale.T("messenger.whoami.none")+locale.T("messenger.telegram.whoami.none_hint")))
		return
	}
	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID,
		fmt.Sprintf(locale.T("messenger.whoami.current"), entry.DisplayName)+
			locale.T("messenger.telegram.whoami.current_hint")))
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
		locale := a.locale(chatID)
		key := "messenger.join_confirm.no_result"
		if confirmed {
			key = "messenger.join_confirm.yes_result"
		}
		edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, locale.T(key))
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

	locale := a.locale(chatID)
	text := fmt.Sprintf(locale.T("messenger.join_confirm.prompt"), meetingID, platform)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		[]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(locale.T("messenger.join_confirm.btn.yes"), "join:yes"),
			tgbotapi.NewInlineKeyboardButtonData(locale.T("messenger.join_confirm.btn.no"), "join:no"),
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

	locale := a.locale(chatID)
	var msg tgbotapi.MessageConfig
	if c.QuestionType == string(challenges.MultipleChoice) && len(c.Choices) > 0 {
		msg = buildMCQMessage(chatID, c)
	} else {
		msg = buildTextMessage(chatID, c, locale)
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

func buildTextMessage(chatID int64, c messengers.ChallengePrompt, locale i18n.Locale) tgbotapi.MessageConfig {
	key := "messenger.challenge.reply_hint.text"
	if c.QuestionType == string(challenges.Numeric) {
		key = "messenger.challenge.reply_hint.numeric"
	}
	return tgbotapi.NewMessage(chatID, c.Prompt+locale.T(key))
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

// updateSender returns the originating Telegram user for an update, or
// nil when none of the supported update kinds apply.
func updateSender(upd tgbotapi.Update) *tgbotapi.User {
	switch {
	case upd.Message != nil:
		return upd.Message.From
	case upd.CallbackQuery != nil:
		return upd.CallbackQuery.From
	}
	return nil
}

// senderChatID returns the chat ID associated with an update for
// language-cache lookup.
func senderChatID(upd tgbotapi.Update) int64 {
	switch {
	case upd.Message != nil:
		return upd.Message.Chat.ID
	case upd.CallbackQuery != nil:
		return upd.CallbackQuery.From.ID
	}
	return 0
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
