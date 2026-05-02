package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"presence-tracker/src/internal/messengers"
)

// CodeGenerator is called when a student sends /start. It must store the
// pairing code internally and return it so the adapter can send it to the student.
type CodeGenerator func(ctx context.Context, handle string) (code string, err error)

// pendingKey identifies a question message whose reply is expected as an answer.
type pendingKey struct {
	chatID    int64
	messageID int
}

// Adapter implements messengers.Messenger using the Telegram Bot API.
type Adapter struct {
	bot           *tgbotapi.BotAPI
	generateCode  CodeGenerator
	pairingPrefix string
	events        chan messengers.Event

	mu         sync.Mutex
	pending    map[pendingKey]string // {chatID, questionMsgID} → challengeID
	pendingInv map[string]pendingKey // challengeID → pendingKey (for cleanup on timeout)
}

// New creates a Telegram adapter. generateCode is called on /start and must
// return the pairing code to send to the student. pairingPrefix is the full
// prefix string used in the meeting chat (e.g. "PTRACK:"), supplied by the
// session coordinator so it is defined in one place.
func New(token string, generateCode CodeGenerator, pairingPrefix string) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	return &Adapter{
		bot:           bot,
		generateCode:  generateCode,
		pairingPrefix: pairingPrefix,
		events:        make(chan messengers.Event, 64),
		pending:       make(map[pendingKey]string),
		pendingInv:    make(map[string]pendingKey),
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
		a.handleStart(ctx, upd.Message)

	case upd.Message != nil && upd.Message.Text != "":
		a.handleTextMessage(upd.Message)

	case upd.CallbackQuery != nil:
		a.handleCallback(upd.CallbackQuery)
	}
}

func (a *Adapter) handleStart(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	handle := strconv.FormatInt(chatID, 10)

	code, err := a.generateCode(ctx, handle)
	if err != nil {
		slog.Error("telegram: generate pairing code", "err", err)
		// TODO: use the configured UI language for this message
		reply := tgbotapi.NewMessage(chatID, "Could not generate pairing code. Please try again later.")
		_, _ = a.bot.Send(reply)
		return
	}

	// TODO: use the configured UI language for this message
	text := fmt.Sprintf(
		"Your pairing code is:\n\n`%s%s`\n\nType this exactly in the meeting chat to complete registration.",
		a.pairingPrefix, code,
	)
	reply := tgbotapi.NewMessage(chatID, text)
	reply.ParseMode = "Markdown"
	if _, err := a.bot.Send(reply); err != nil {
		slog.Error("telegram: send pairing code", "err", err)
		return
	}

	a.events <- messengers.Event{
		Kind:      messengers.EventKindPairingStarted,
		Handle:    handle,
		Timestamp: time.Now().UTC(),
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
	// Callback data format: "<challengeID>:<choiceLabel>"
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 {
		return
	}
	cid, choice := parts[0], parts[1]
	chatID := cq.From.ID
	handle := strconv.FormatInt(chatID, 10)

	// Acknowledge the button press immediately so Telegram stops showing the spinner.
	ack := tgbotapi.NewCallback(cq.ID, "")
	_, _ = a.bot.Request(ack)

	// AnswerMessageRef is empty for MCQ callbacks — there is no reply message to delete.
	a.events <- messengers.Event{
		Kind:        messengers.EventKindAnswerReceived,
		Handle:      handle,
		ChallengeID: cid,
		Answer:      choice,
		Selected:    []string{choice},
		Timestamp:   time.Now().UTC(),
	}
}

// SendChallenge sends a challenge prompt to a student. For multiple_choice
// questions, inline keyboard buttons are shown (one per choice). For other
// types, a plain text prompt is sent and the student must reply to that message.
func (a *Adapter) SendChallenge(ctx context.Context, handle string, c messengers.ChallengePrompt) (messengers.MessageRef, error) {
	chatID, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: invalid handle %q: %w", handle, err)
	}

	var msg tgbotapi.MessageConfig
	if c.QuestionType == "multiple_choice" && len(c.Choices) > 0 {
		msg = buildMCQMessage(chatID, c)
	} else {
		msg = buildTextMessage(chatID, c)
	}

	sent, err := a.bot.Send(msg)
	if err != nil {
		return messengers.MessageRef{}, fmt.Errorf("telegram: send challenge: %w", err)
	}

	// Register the challenge so the student's reply is routed here.
	if c.QuestionType != "multiple_choice" {
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
	if c.QuestionType == "numeric" {
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
