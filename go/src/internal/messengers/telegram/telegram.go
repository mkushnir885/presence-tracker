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

const registerPromptTTL = 60 * time.Second

const languagePromptTTL = 60 * time.Second

const (
	Name        = "telegram"
	DisplayName = "Telegram"
)

func init() {
	messengers.Register(Name, DisplayName)
}

type pendingKey struct {
	chatID    int64
	messageID int
}

type mcqState struct {
	challengeID string
	choices     []string
	selected    []bool
}

type Adapter struct {
	bot      *tgbotapi.BotAPI
	registry participants.Registry
	events   chan messengers.Event
	catalog  *i18n.Catalog

	mu      sync.Mutex
	pending map[pendingKey]string    // question message → challenge awaiting a text/numeric reply
	mcq     map[pendingKey]*mcqState // multiple-choice message → in-flight selection state

	stopOnce sync.Once // guards bot.StopReceivingUpdates against a double close

	// registerPrompts / languagePrompts hold the message ID of an outstanding
	// prompt per chat; the entry expires (auto-deleting the prompt) if the
	// user never replies.
	registerPrompts *util.TTLMap[int64, int]
	languagePrompts *util.TTLMap[int64, int]
}

func New(token string, registry participants.Registry) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	a := &Adapter{
		bot:      bot,
		registry: registry,
		events:   make(chan messengers.Event, 64),
		catalog:  newCatalog(),
		pending:  make(map[pendingKey]string),
		mcq:      make(map[pendingKey]*mcqState),
	}
	a.registerPrompts = util.NewTTLMap(a.deleteMessageByID)
	a.languagePrompts = util.NewTTLMap(a.deleteMessageByID)
	return a, nil
}

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

func (a *Adapter) localeFor(lang string) i18n.Locale {
	if lang == "" {
		return a.catalog.Locale("en")
	}
	return a.catalog.Locale(lang)
}

func (a *Adapter) Name() string        { return Name }
func (a *Adapter) DisplayName() string { return DisplayName }

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

// skipBacklog acks every update queued while the bot was offline:
// getUpdates with offset=-1 returns only the latest pending update, and
// using its ID+1 as the next offset discards all earlier ones.
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

func (a *Adapter) Stop(_ context.Context) error {
	a.stopReceiving()
	return nil
}

// stopReceiving is idempotent: the upstream library closes an internal
// channel on each call and panics on the second close.
func (a *Adapter) stopReceiving() {
	a.stopOnce.Do(a.bot.StopReceivingUpdates)
}

func (a *Adapter) publishCommands() {
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
	case upd.Message != nil && upd.Message.IsCommand():
		a.handleCommand(ctx, upd.Message)
	case upd.Message != nil && upd.Message.Text != "":
		a.handleTextMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil:
		a.handleCallback(ctx, upd.CallbackQuery)
	}
}

func (a *Adapter) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		a.handleStart(msg)
	case "register":
		a.handleRegister(msg)
	case "unregister":
		a.handleUnregister(ctx, msg)
	case "whoami":
		a.handleWhoami(msg)
	case "language":
		a.handleLanguage(msg)
	}
}

func (a *Adapter) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	locale := a.locale(chatID, msg.From.LanguageCode)
	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, locale.T("messenger.telegram.start")))
}

func (a *Adapter) handleRegister(msg *tgbotapi.Message) {
	a.promptRegister(msg.Chat.ID, msg.From.LanguageCode)
}

// promptRegister tracks the prompt message ID so the next reply to it is
// treated as a registration. A ForceReply marker is deliberately not
// attached: clients re-apply it every time the chat opens, so a stale
// prompt would keep nagging the user long after registration.
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

func (a *Adapter) discardLanguagePrompt(chatID int64) {
	a.languagePrompts.Delete(chatID)
}

func (a *Adapter) handleLanguageCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	chosen := strings.TrimPrefix(cq.Data, "lang:")
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	a.discardLanguagePrompt(chatID)

	updated, err := a.registry.SetLanguage(ctx, a.Name(), handle, chosen)
	if err != nil {
		slog.Warn("telegram: set language", "chat_id", chatID, "lang", chosen, "err", err)
		return
	}
	if !updated {
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

func (a *Adapter) editClearKeyboard(chatID int64, messageID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	empty := emptyInlineKeyboard()
	edit.ReplyMarkup = &empty
	if _, err := a.bot.Send(edit); err != nil {
		slog.Debug("telegram: edit message", "chat_id", chatID, "msg_id", messageID, "err", err)
	}
}

// emptyInlineKeyboard strips a keyboard on edit. The field must be an
// empty array, not null — Telegram rejects null inline_keyboard — so the
// upstream NewInlineKeyboardMarkup() helper (which leaves it nil) cannot
// be used here.
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

func (a *Adapter) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	switch {
	case strings.HasPrefix(cq.Data, "join:"):
		a.handleConfirmationCallback(cq)
	case strings.HasPrefix(cq.Data, "lang:"):
		a.handleLanguageCallback(ctx, cq)
	case strings.HasPrefix(cq.Data, "mcq:"):
		a.handleMCQCallback(cq)
	}
}

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

func (a *Adapter) SendJoinConfirmation(_ context.Context, handle, lang, meetingID, platform string) (messengers.MessageRef, error) {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}

	locale := a.localeFor(lang)
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

func (a *Adapter) SendChallenge(ctx context.Context, handle, lang string, c messengers.ChallengePrompt) (messengers.MessageRef, error) {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}

	locale := a.localeFor(lang)
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

func renderMCQKeyboard(choices []string, selected []bool, locale i18n.Locale) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(choices)+1)
	for i, choice := range choices {
		prefix := "⬜ "
		if selected[i] {
			prefix = "✅ "
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

var notifyKeys = map[messengers.NotifyKind]string{
	messengers.NotifyJoinDropped:       "messenger.join_confirm.dropped",
	messengers.NotifyJoinTimedOut:      "messenger.join_confirm.timed_out",
	messengers.NotifyJoinCollision:     "messenger.join_confirm.collision",
	messengers.NotifyChallengeAnswered: "messenger.challenge.answered",
	messengers.NotifyChallengeTimedOut: "messenger.challenge.timed_out",
}

func (a *Adapter) Notify(_ context.Context, ref messengers.MessageRef, lang string, kind messengers.NotifyKind, args ...any) error {
	var r telegramRef
	if err := json.Unmarshal([]byte(ref.Opaque), &r); err != nil {
		return fmt.Errorf("telegram: decode ref: %w", err)
	}
	key, ok := notifyKeys[kind]
	if !ok {
		return fmt.Errorf("telegram: unknown notify kind %d", kind)
	}
	a.discardMCQState(r.ChatID, r.MessageID)
	locale := a.localeFor(lang)
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

func (a *Adapter) SendNotification(_ context.Context, handle, lang string, kind messengers.NotifyKind, args ...any) error {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}
	key, ok := notifyKeys[kind]
	if !ok {
		return fmt.Errorf("telegram: unknown notify kind %d", kind)
	}
	locale := a.localeFor(lang)
	text := locale.T(key)
	if len(args) > 0 {
		text = fmt.Sprintf(text, args...)
	}
	_, err = a.bot.Send(tgbotapi.NewMessage(chatID, text))
	return err
}

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
