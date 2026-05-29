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

// registerPromptTTL is how long an outstanding /register prompt waits
// for the user's display-name message before being auto-deleted. Picked
// to be long enough for a slow typer on mobile, short enough that an
// abandoned prompt disappears within the same lesson.
const registerPromptTTL = 60 * time.Second

// languagePromptTTL is how long the language picker waits for a tap
// before its message is deleted. Matches registerPromptTTL: the two
// pickers have similar "abandoned dialog" semantics.
const languagePromptTTL = 60 * time.Second

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

// mcqState holds the selection state for one in-flight multiple-choice
// question. selected[i] indicates whether choice i is currently picked;
// the slice is rebuilt into a keyboard markup on every toggle, then
// finalised when the user taps Submit.
type mcqState struct {
	challengeID string
	choices     []string
	selected    []bool
}

// Adapter implements messengers.Messenger using the Telegram Bot API.
type Adapter struct {
	bot      *tgbotapi.BotAPI
	registry participants.Registry
	events   chan messengers.Event
	catalog  *i18n.Catalog

	mu         sync.Mutex
	pending    map[pendingKey]string    // {chatID, questionMsgID} → challengeID
	pendingInv map[string]pendingKey    // challengeID → pendingKey (for cleanup on timeout)
	mcq        map[pendingKey]*mcqState // {chatID, MCQ msgID} → toggle/submit state

	// stopOnce guards bot.StopReceivingUpdates: both the Start goroutine
	// (on ctx cancel) and an explicit Stop call race to shut the poller
	// down, and the upstream library panics on a double close.
	stopOnce sync.Once

	// registerPrompts tracks the message ID of an outstanding /register
	// prompt per chat. Its presence marks the chat as awaiting a
	// display-name message. When the display name arrives the entry is
	// removed but the prompt is left in chat as a record of the flow;
	// if no reply arrives the entry expires and the prompt is
	// auto-deleted so it doesn't keep nagging the user.
	registerPrompts *util.TTLMap[int64, int]

	// languagePrompts tracks the message ID of an outstanding /language
	// inline-keyboard picker per chat. Entries are removed on selection
	// (so no auto-delete fires) and expire after languagePromptTTL with
	// the picker auto-deleted if the user never tapped.
	languagePrompts *util.TTLMap[int64, int]
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
		mcq:        make(map[pendingKey]*mcqState),
	}
	a.registerPrompts = util.NewTTLMap(a.deleteMessageByID)
	a.languagePrompts = util.NewTTLMap(a.deleteMessageByID)
	return a, nil
}

// discardRegisterPrompt removes the outstanding /register prompt for
// chatID (if any) from both the TTL map and the chat itself, so the
// chat history stays clean once the registration is complete or
// abandoned.
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

	offset := a.skipBacklog()
	u := tgbotapi.NewUpdate(offset)
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

// skipBacklog returns the offset to use for the live update loop so
// that any messages sent while the bot was offline are discarded.
// Telegram's getUpdates with offset=-1 returns only the most recent
// pending update, and using that update's ID + 1 as the next offset
// implicitly acknowledges every earlier one. Returns 0 (no skip) if
// the call fails or no backlog exists.
func (a *Adapter) skipBacklog() int {
	drain := tgbotapi.NewUpdate(-1)
	drain.Timeout = 0
	recent, err := a.bot.GetUpdates(drain)
	if err != nil {
		slog.Warn("telegram: drain backlog", "err", err)
		return 0
	}
	if len(recent) == 0 {
		return 0
	}
	last := recent[len(recent)-1].UpdateID
	slog.Info("telegram: skipped offline backlog", "through_update_id", last)
	return last + 1
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

// handleRegister always opens the registration prompt; the display
// name is collected from the user's reply, never from inline
// arguments.
func (a *Adapter) handleRegister(msg *tgbotapi.Message) {
	a.promptRegister(msg.Chat.ID, msg.From.LanguageCode)
}

// promptRegister sends a plain prompt asking the user to reply with
// their display name. A ForceReply marker is intentionally not
// attached: Telegram clients re-apply ForceReply UI every time the
// chat is opened, which made stale prompts (e.g. left over after a
// daemon restart) keep nagging the user to reply long after
// registration was complete. The prompt message ID is tracked per
// chat; the next reply to it is dispatched as a registration. Any
// previous outstanding prompt is deleted first so the chat carries
// one prompt at most.
func (a *Adapter) promptRegister(chatID int64, langHint string) {
	a.discardRegisterPrompt(chatID)
	locale := a.locale(chatID, langHint)
	prompt := tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.register.prompt"))
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
	a.discardLanguagePrompt(chatID)
	locale := a.locale(chatID, msg.From.LanguageCode)
	prompt := tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.language.prompt"))
	row := make([]tgbotapi.InlineKeyboardButton, 0, len(supportedLanguages))
	for _, l := range supportedLanguages {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(l.label, "lang:"+l.code))
	}
	prompt.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(row)
	sent, err := a.bot.Send(prompt)
	if err != nil {
		slog.Warn("telegram: send language prompt", "err", err)
		return
	}
	a.languagePrompts.Put(chatID, sent.MessageID, languagePromptTTL)
}

// discardLanguagePrompt removes the outstanding /language picker for
// chatID (if any) from the TTL map without deleting the message —
// callers that handle the tap edit the picker in place rather than
// deleting it.
func (a *Adapter) discardLanguagePrompt(chatID int64) {
	a.languagePrompts.Delete(chatID)
}

func (a *Adapter) handleLanguageCallback(cq *tgbotapi.CallbackQuery) {
	chosen := strings.TrimPrefix(cq.Data, "lang:")
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	// User tapped — the picker is no longer "hanging", so cancel the
	// auto-delete. The message is then edited in place below.
	a.discardLanguagePrompt(chatID)

	updated, err := a.registry.SetLanguage(context.Background(), a.Name(), handle, chosen)
	if err != nil {
		slog.Warn("telegram: set language", "chat_id", chatID, "lang", chosen, "err", err)
		return
	}
	if !updated {
		// Not registered yet; reply in the user's currently effective language.
		locale := a.locale(chatID, cq.From.LanguageCode)
		if cq.Message != nil {
			a.editClearKeyboard(chatID, cq.Message.MessageID,
				locale.T("messenger.telegram.language.unregistered"))
		}
		return
	}

	if cq.Message != nil {
		newLocale := a.catalog.Locale(chosen)
		a.editClearKeyboard(chatID, cq.Message.MessageID,
			newLocale.T("messenger.telegram.language.confirm"))
	}
}

// editClearKeyboard rewrites a previously-sent message and drops any
// inline keyboard attached to it. Telegram's edit_message_text requires
// reply_markup to be an array (not null), so the empty keyboard is
// explicitly materialized as an empty slice.
func (a *Adapter) editClearKeyboard(chatID int64, messageID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	empty := emptyInlineKeyboard()
	edit.ReplyMarkup = &empty
	if _, err := a.bot.Send(edit); err != nil {
		slog.Debug("telegram: edit message", "chat_id", chatID, "msg_id", messageID, "err", err)
	}
}

// emptyInlineKeyboard returns a markup whose inline_keyboard field is
// an empty array (not null). Telegram rejects null with
// `Bad Request: field "inline_keyboard" must be of type Array`, so the
// upstream NewInlineKeyboardMarkup() helper (which leaves the slice nil
// when no rows are passed) cannot be used to strip an existing keyboard.
func emptyInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
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
			return
		}
		a.registerPrompts.Delete(chatID)
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
	case strings.HasPrefix(cq.Data, "mcq:"):
		a.handleMCQCallback(cq)
	}
}

// handleMCQCallback dispatches a tap on an MCQ keyboard. Data format
// is "mcq:t:<idx>" for a choice toggle and "mcq:s" for Submit. The
// challenge and its choice list are looked up by (chat, message) from
// the adapter's mcq map; the callback_data carries only the row tag
// and index so it stays well under Telegram's 64-byte limit even for
// long choice strings.
func (a *Adapter) handleMCQCallback(cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil {
		_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))
		return
	}
	chatID := cq.Message.Chat.ID
	key := pendingKey{chatID: chatID, messageID: cq.Message.MessageID}

	a.mu.Lock()
	state, ok := a.mcq[key]
	a.mu.Unlock()
	if !ok {
		// Message already finalised (answered, timed out, or deleted).
		_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))
		return
	}

	locale := a.locale(chatID, cq.From.LanguageCode)
	payload := strings.TrimPrefix(cq.Data, "mcq:")
	switch {
	case payload == "s":
		a.submitMCQ(cq, key, state, locale)
	case strings.HasPrefix(payload, "t:"):
		idx, err := strconv.Atoi(strings.TrimPrefix(payload, "t:"))
		if err != nil || idx < 0 || idx >= len(state.choices) {
			_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))
			return
		}
		a.toggleMCQ(cq, key, state, idx, locale)
	default:
		_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))
	}
}

func (a *Adapter) toggleMCQ(cq *tgbotapi.CallbackQuery, key pendingKey, state *mcqState, idx int, locale i18n.Locale) {
	a.mu.Lock()
	state.selected[idx] = !state.selected[idx]
	snapshot := make([]bool, len(state.selected))
	copy(snapshot, state.selected)
	a.mu.Unlock()

	_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))

	kb := renderMCQKeyboard(state.choices, snapshot, locale)
	edit := tgbotapi.NewEditMessageReplyMarkup(key.chatID, key.messageID, kb)
	if _, err := a.bot.Send(edit); err != nil {
		slog.Debug("telegram: edit MCQ keyboard", "chat_id", key.chatID, "msg_id", key.messageID, "err", err)
	}
}

func (a *Adapter) submitMCQ(cq *tgbotapi.CallbackQuery, key pendingKey, state *mcqState, locale i18n.Locale) {
	a.mu.Lock()
	selected := make([]string, 0, len(state.selected))
	for i, on := range state.selected {
		if on {
			selected = append(selected, state.choices[i])
		}
	}
	a.mu.Unlock()

	if len(selected) == 0 {
		ack := tgbotapi.NewCallback(cq.ID, locale.T("messenger.telegram.mcq.empty_alert"))
		ack.ShowAlert = true
		_, _ = a.bot.Request(ack)
		return
	}

	_, _ = a.bot.Request(tgbotapi.NewCallback(cq.ID, ""))

	a.mu.Lock()
	delete(a.mcq, key)
	a.mu.Unlock()

	a.events <- messengers.Event{
		Kind:        messengers.EventKindAnswerReceived,
		Handle:      strconv.FormatInt(key.chatID, 10),
		ChallengeID: state.challengeID,
		Answer:      strings.Join(selected, ", "),
		Selected:    selected,
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
		empty := emptyInlineKeyboard()
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
	isMCQ := c.QuestionType == string(challenges.MultipleChoice) && len(c.Choices) > 0
	var msg tgbotapi.MessageConfig
	if isMCQ {
		msg = buildMCQMessage(chatID, c, locale)
	} else {
		msg = buildTextMessage(chatID, c, locale)
	}

	sent, err := a.bot.Send(msg)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: send challenge: %w", err)
	}

	key := pendingKey{chatID: chatID, messageID: sent.MessageID}
	a.mu.Lock()
	if isMCQ {
		a.mcq[key] = &mcqState{
			challengeID: c.ChallengeID,
			choices:     c.Choices,
			selected:    make([]bool, len(c.Choices)),
		}
	} else {
		a.pending[key] = c.ChallengeID
		a.pendingInv[c.ChallengeID] = key
	}
	a.mu.Unlock()

	ref := telegramRef{ChatID: chatID, MessageID: sent.MessageID}
	b, _ := json.Marshal(ref) //nolint:errchkjson // telegramRef is a plain int64 struct; Marshal cannot fail
	return messengers.MessageRef{Opaque: string(b)}, nil
}

func buildMCQMessage(chatID int64, c messengers.ChallengePrompt, locale i18n.Locale) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, c.Prompt+locale.T("messenger.telegram.mcq.hint"))
	msg.ReplyMarkup = renderMCQKeyboard(c.Choices, make([]bool, len(c.Choices)), locale)
	return msg
}

// renderMCQKeyboard builds the inline keyboard for an MCQ question.
// Each choice row carries a ☐/☑ prefix reflecting whether it is
// currently selected; a final Submit row finalises the answer.
func renderMCQKeyboard(choices []string, selected []bool, locale i18n.Locale) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(choices)+1)
	for i, choice := range choices {
		prefix := "☐ "
		if selected[i] {
			prefix = "☑ "
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(prefix+choice, "mcq:t:"+strconv.Itoa(i)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData(locale.T("messenger.telegram.mcq.submit"), "mcq:s"),
	})
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
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
	a.discardMCQState(r.ChatID, r.MessageID)
	locale := a.locale(r.ChatID, "")
	text := locale.T(key)
	if len(args) > 0 {
		text = fmt.Sprintf(text, args...)
	}
	edit := tgbotapi.NewEditMessageText(r.ChatID, r.MessageID, text)
	empty := emptyInlineKeyboard()
	edit.ReplyMarkup = &empty
	_, err := a.bot.Send(edit)
	return err
}

// SendNotification sends a fresh localized message keyed by kind. Used
// for receipt-style follow-ups (e.g. "answer saved") whose lifetime is
// independent of the message being acknowledged.
func (a *Adapter) SendNotification(_ context.Context, handle string, kind messengers.NotifyKind, args ...any) error {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}
	key, ok := notifyKeys[kind]
	if !ok {
		return fmt.Errorf("telegram: unknown notify kind %d", kind)
	}
	locale := a.locale(chatID, "")
	text := locale.T(key)
	if len(args) > 0 {
		text = fmt.Sprintf(text, args...)
	}
	_, err = a.bot.Send(tgbotapi.NewMessage(chatID, text))
	return err
}

// DeleteMessage deletes a previously sent message.
func (a *Adapter) DeleteMessage(_ context.Context, ref messengers.MessageRef) error {
	var r telegramRef
	if err := json.Unmarshal([]byte(ref.Opaque), &r); err != nil {
		return fmt.Errorf("telegram: decode ref: %w", err)
	}
	a.discardMCQState(r.ChatID, r.MessageID)
	del := tgbotapi.NewDeleteMessage(r.ChatID, r.MessageID)
	_, err := a.bot.Request(del)
	return err
}

// discardMCQState drops any pending MCQ selection state for the given
// message. Called when the question is finalised (answered, timed out,
// or deleted) so stale entries do not linger after the keyboard is
// gone from the chat.
func (a *Adapter) discardMCQState(chatID int64, messageID int) {
	key := pendingKey{chatID: chatID, messageID: messageID}
	a.mu.Lock()
	delete(a.mcq, key)
	a.mu.Unlock()
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
