package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/i18n"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/util"
)

// registerPromptTTL is how long an outstanding /register ForceReply
// prompt waits before being auto-deleted. Picked to be long enough for
// a slow typer on mobile, short enough that an abandoned prompt
// disappears within the same lesson.
const registerPromptTTL = 60 * time.Second

// languageConfirmTTL is how long the "Language set to …" / "Register
// first" reply lingers in chat after the user taps a language button.
// Long enough to read, short enough to keep the bot's history tidy.
const languageConfirmTTL = 8 * time.Second

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

	// stopOnce guards bot.StopReceivingUpdates: both the Start goroutine
	// (on ctx cancel) and an explicit Stop call race to shut the poller
	// down, and the upstream library panics on a double close.
	stopOnce sync.Once

	// registerPrompts tracks the message ID of an outstanding /register
	// ForceReply prompt per chat. Entries expire after registerPromptTTL;
	// the prompt message is auto-deleted by the onExpire callback so an
	// abandoned dialog does not linger.
	registerPrompts *util.TTLMap[int64, int]
}

// New creates a Telegram adapter.
// registry is called directly by /register command handlers.
func New(token string, registry participants.Registry) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	a := &Adapter{
		bot:        bot,
		registry:   registry,
		events:     make(chan messengers.Event, 64),
		catalog:    newCatalog(),
		pending:    make(map[pendingKey]string),
		pendingInv: make(map[string]pendingKey),
	}
	a.registerPrompts = util.NewTTLMap(a.expireRegisterPrompt)
	return a, nil
}

// expireRegisterPrompt deletes the stale /register prompt message in
// chatID. Best-effort: the message may already be gone (user dismissed
// or replied just as the TTL fired), and either way the entry is
// already removed from the map by util.TTLMap before this callback
// runs.
func (a *Adapter) expireRegisterPrompt(chatID int64, messageID int) {
	a.deleteMessageByID(chatID, messageID)
}

// discardRegisterPrompt removes the outstanding /register prompt for
// chatID (if any) from both the TTL map and the chat itself. Telegram
// re-applies a message's ForceReply markup every time the user opens
// the chat, so leaving the prompt message in place after the reply has
// been processed makes the input box keep nagging the user to "Reply
// with your display name" — even though, from the bot's perspective,
// registration is already complete. Always deleting the message closes
// that gap.
func (a *Adapter) discardRegisterPrompt(chatID int64) {
	if msgID, ok := a.registerPrompts.Delete(chatID); ok {
		a.deleteMessageByID(chatID, msgID)
	}
}

func (a *Adapter) deleteMessageByID(chatID int64, messageID int) {
	del := tgbotapi.NewDeleteMessage(chatID, messageID)
	if _, err := a.bot.Request(del); err != nil {
		slog.Debug("telegram: delete message", "chat_id", chatID, "msg_id", messageID, "err", err)
	}
}

// locale returns the Locale for chatID. The registry-persisted
// preference always wins so a user's explicit /language choice is
// never overridden by their device's UI language. When no entry
// exists (an unregistered user typing /start) hint — typically the
// inbound message's language_code — is used; if hint is empty too,
// fall back to English.
func (a *Adapter) locale(chatID int64, hint string) i18n.Locale {
	handle := strconv.FormatInt(chatID, 10)
	if entry, ok := a.registry.LookupByHandle(a.Name(), handle); ok && entry.Language != "" {
		return a.catalog.Locale(entry.Language)
	}
	if hint != "" {
		return a.catalog.Locale(languageFromCode(hint))
	}
	return a.catalog.Locale("en")
}

func (a *Adapter) Name() string { return Name }

// Start begins polling the Telegram API for updates. The returned channel is
// closed when ctx is cancelled.
func (a *Adapter) Start(ctx context.Context) (<-chan messengers.Event, error) {
	a.publishCommands()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := a.bot.GetUpdatesChan(u)

	go func() {
		defer close(a.events)
		for {
			select {
			case <-ctx.Done():
				a.stopReceiving()
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
	a.stopReceiving()
	return nil
}

// stopReceiving is the idempotent gate around bot.StopReceivingUpdates;
// the upstream library closes an internal channel each time it is called
// and panics on the second close.
func (a *Adapter) stopReceiving() {
	a.stopOnce.Do(a.bot.StopReceivingUpdates)
}

// publishCommands registers the bot's command list with Telegram so
// the in-app slash-menu autocompletes /register, /whoami, /unregister
// with localized descriptions. Telegram persists the list server-side
// per (scope, language), so this is sent once at startup. Failures are
// cosmetic — the bot still works without the menu — so they are logged
// rather than propagated.
func (a *Adapter) publishCommands() {
	// Telegram language code → catalog language. The empty Telegram
	// code is the default fallback for any client whose interface
	// language is not explicitly registered below.
	targets := map[string]string{
		"":   "en",
		"uk": "uk",
	}
	for tgLang, catLang := range targets {
		loc := a.catalog.Locale(catLang)
		cfg := tgbotapi.NewSetMyCommandsWithScopeAndLanguage(
			tgbotapi.NewBotCommandScopeDefault(), tgLang,
			tgbotapi.BotCommand{Command: "register", Description: loc.T("messenger.telegram.commands.register")},
			tgbotapi.BotCommand{Command: "whoami", Description: loc.T("messenger.telegram.commands.whoami")},
			tgbotapi.BotCommand{Command: "unregister", Description: loc.T("messenger.telegram.commands.unregister")},
			tgbotapi.BotCommand{Command: "language", Description: loc.T("messenger.telegram.commands.language")},
		)
		if _, err := a.bot.Request(cfg); err != nil {
			slog.Warn("telegram: publish commands", "lang", tgLang, "err", err)
		}
	}
}

func (a *Adapter) handleUpdate(ctx context.Context, upd tgbotapi.Update) {
	switch {
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "start":
		a.handleStart(upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "register":
		a.handleRegister(upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "unregister":
		a.handleUnregister(ctx, upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "whoami":
		a.handleWhoami(upd.Message)
	case upd.Message != nil && upd.Message.IsCommand() && upd.Message.Command() == "language":
		a.handleLanguage(upd.Message)
	case upd.Message != nil && upd.Message.Text != "":
		a.handleTextMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil:
		a.handleCallback(upd.CallbackQuery)
	}
}

func (a *Adapter) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	locale := a.locale(chatID, msg.From.LanguageCode)
	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.start")))
}

// handleRegister always opens the ForceReply prompt; the display name
// is collected from the user's reply, never from inline arguments.
func (a *Adapter) handleRegister(msg *tgbotapi.Message) {
	a.promptRegister(msg.Chat.ID, msg.From.LanguageCode)
}

// promptRegister sends a ForceReply prompt asking the user for their
// display name. Telegram focuses the input as a reply to this message,
// so the user types only the name. The prompt's message ID is tracked
// per chat; the next reply to it is dispatched as a registration. Any
// previous outstanding prompt is deleted first so the chat never holds
// more than one active ForceReply marker.
func (a *Adapter) promptRegister(chatID int64, langHint string) {
	a.discardRegisterPrompt(chatID)
	locale := a.locale(chatID, langHint)
	prompt := tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.register.prompt"))
	prompt.ReplyMarkup = tgbotapi.ForceReply{
		ForceReply:            true,
		InputFieldPlaceholder: locale.T("messenger.telegram.register.placeholder"),
		Selective:             true,
	}
	sent, err := a.bot.Send(prompt)
	if err != nil {
		slog.Warn("telegram: send register prompt", "err", err)
		return
	}
	a.registerPrompts.Put(chatID, sent.MessageID, registerPromptTTL)
}

func (a *Adapter) registerDisplayName(ctx context.Context, msg *tgbotapi.Message, displayName string) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)
	label := userLabel(msg.From)
	language := languageFromCode(msg.From.LanguageCode)
	locale := a.locale(chatID, msg.From.LanguageCode)

	err := a.registry.Register(ctx, a.Name(), handle, label, displayName, language)
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
	locale := a.locale(chatID, msg.From.LanguageCode)

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

// supportedLanguages lists the catalog languages users can switch to,
// in the order they appear in the inline keyboard. The display label
// is each language's own endonym so the picker is readable regardless
// of which language is currently active.
var supportedLanguages = []struct {
	code  string
	label string
}{
	{"en", "English"},
	{"uk", "Українська"},
}

func (a *Adapter) handleLanguage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	locale := a.locale(chatID, msg.From.LanguageCode)
	prompt := tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.language.prompt"))
	row := make([]tgbotapi.InlineKeyboardButton, 0, len(supportedLanguages))
	for _, l := range supportedLanguages {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(l.label, "lang:"+l.code))
	}
	prompt.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(row)
	_, _ = a.bot.Send(prompt)
}

func (a *Adapter) handleLanguageCallback(cq *tgbotapi.CallbackQuery) {
	chosen := strings.TrimPrefix(cq.Data, "lang:")
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	updated, err := a.registry.SetLanguage(context.Background(), a.Name(), handle, chosen)
	if err != nil {
		slog.Warn("telegram: set language", "chat_id", chatID, "lang", chosen, "err", err)
		return
	}
	if !updated {
		// Not registered yet; reply in the user's currently effective language.
		locale := a.locale(chatID, cq.From.LanguageCode)
		if cq.Message != nil {
			a.editAndAutoDelete(chatID, cq.Message.MessageID,
				locale.T("messenger.telegram.language.unregistered"), languageConfirmTTL)
		}
		return
	}

	if cq.Message != nil {
		newLocale := a.catalog.Locale(chosen)
		a.editAndAutoDelete(chatID, cq.Message.MessageID,
			newLocale.T("messenger.telegram.language.confirm"), languageConfirmTTL)
	}
}

// editAndAutoDelete rewrites a previously-sent message (clearing any
// inline keyboard) and schedules its deletion after ttl. Used by the
// language picker so its confirmation reply does not pile up in the
// chat between sessions.
func (a *Adapter) editAndAutoDelete(chatID int64, messageID int, text string, ttl time.Duration) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	empty := tgbotapi.NewInlineKeyboardMarkup()
	edit.ReplyMarkup = &empty
	if _, err := a.bot.Send(edit); err != nil {
		slog.Debug("telegram: edit before auto-delete", "chat_id", chatID, "msg_id", messageID, "err", err)
		return
	}
	time.AfterFunc(ttl, func() { a.deleteMessageByID(chatID, messageID) })
}

func (a *Adapter) handleWhoami(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)
	locale := a.locale(chatID, msg.From.LanguageCode)
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

func (a *Adapter) handleTextMessage(ctx context.Context, msg *tgbotapi.Message) {
	// Only replies are meaningful — either to a /register prompt or to a question message.
	if msg.ReplyToMessage == nil {
		return
	}

	chatID := msg.Chat.ID
	replyTo := msg.ReplyToMessage.MessageID

	if promptID, ok := a.registerPrompts.Get(chatID); ok && replyTo == promptID {
		name := strings.TrimSpace(msg.Text)
		if name == "" {
			// promptRegister discards the stale prompt before sending the new one.
			a.promptRegister(chatID, msg.From.LanguageCode)
			return
		}
		a.discardRegisterPrompt(chatID)
		a.registerDisplayName(ctx, msg, name)
		return
	}

	key := pendingKey{chatID: chatID, messageID: replyTo}

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
	switch {
	case strings.HasPrefix(cq.Data, "join:"):
		a.handleConfirmationCallback(cq)
	case strings.HasPrefix(cq.Data, "lang:"):
		a.handleLanguageCallback(cq)
	default:
		a.handleChallengeCallback(cq)
	}
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
		locale := a.locale(chatID, cq.From.LanguageCode)
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

	locale := a.locale(chatID, "")
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

	locale := a.locale(chatID, "")
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

// notifyKeys maps every messengers.NotifyKind to its locale key. Kept
// next to the Notify implementation so adding a new kind surfaces here.
var notifyKeys = map[messengers.NotifyKind]string{
	messengers.NotifyJoinDropped:       "messenger.join_confirm.dropped",
	messengers.NotifyJoinTimedOut:      "messenger.join_confirm.timed_out",
	messengers.NotifyJoinCollision:     "messenger.join_confirm.collision",
	messengers.NotifyChallengeAnswered: "messenger.challenge.answered",
	messengers.NotifyChallengeTimedOut: "messenger.challenge.timed_out",
}

// Notify edits a previously-sent message to the localized text for
// kind. The recipient's locale is resolved from the chat associated
// with ref; args are forwarded to fmt.Sprintf so kinds whose template
// contains format verbs (e.g. NotifyJoinCollision's %q) render with
// the caller-supplied values.
func (a *Adapter) Notify(_ context.Context, ref messengers.MessageRef, kind messengers.NotifyKind, args ...any) error {
	var r telegramRef
	if err := json.Unmarshal([]byte(ref.Opaque), &r); err != nil {
		return fmt.Errorf("telegram: decode ref: %w", err)
	}
	key, ok := notifyKeys[kind]
	if !ok {
		return fmt.Errorf("telegram: unknown notify kind %d", kind)
	}
	locale := a.locale(r.ChatID, "")
	text := locale.T(key)
	if len(args) > 0 {
		text = fmt.Sprintf(text, args...)
	}
	edit := tgbotapi.NewEditMessageText(r.ChatID, r.MessageID, text)
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
