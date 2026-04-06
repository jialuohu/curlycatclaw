package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/mdhtml"
)

const (
	// maxMessageLen is Telegram's hard limit per message (in Unicode code points).
	maxMessageLen = 4096

	// sendInterval is the minimum delay between outgoing messages to stay
	// well under Telegram's ~30 msgs/sec rate limit.
	sendInterval = 50 * time.Millisecond

	// channelBuffer is the capacity for both the inbox and updates channels.
	channelBuffer = 64

	// maxDownloadBytes is the maximum file size to download from Telegram (20 MiB).
	maxDownloadBytes = 20 << 20
)

// AttachmentKind identifies the type of a media attachment.
type AttachmentKind int

const (
	AttachPhoto    AttachmentKind = iota // image (JPEG, PNG, etc.)
	AttachDocument                       // file/document (PDF, text, binary)
	AttachVoice                          // voice message (.ogg Opus)
	AttachAudio                          // audio file (MP3, etc.)
)

// Attachment represents any media attached to a Telegram message.
type Attachment struct {
	Kind     AttachmentKind
	Data     []byte // raw bytes (downloaded from Telegram)
	MimeType string // e.g. "image/jpeg", "application/pdf"
	FileName string // original filename (documents only)
	FileID   string // Telegram file_id for reference storage
	Duration int    // duration in seconds (voice/audio only)
}

// IncomingMessage represents a message received from a Telegram user.
// It may contain text, attachments (photos, documents, voice), or both.
type IncomingMessage struct {
	ChatID    int64
	UserID    int64
	Text      string
	MessageID int
	// ChatType is the Telegram chat type: "private", "group", "supergroup", or "channel".
	ChatType string
	// Attachments contains downloaded media from the message.
	Attachments []Attachment
}

// Photos returns photo attachments for backward compatibility.
func (m IncomingMessage) Photos() []Attachment {
	var out []Attachment
	for _, a := range m.Attachments {
		if a.Kind == AttachPhoto {
			out = append(out, a)
		}
	}
	return out
}

// Documents returns document attachments.
func (m IncomingMessage) Documents() []Attachment {
	var out []Attachment
	for _, a := range m.Attachments {
		if a.Kind == AttachDocument {
			out = append(out, a)
		}
	}
	return out
}

// InlineButton represents a single button in a Telegram inline keyboard.
type InlineButton struct {
	Text         string // button label
	CallbackData string // data sent back when pressed
}

// OutgoingMessage represents a message to be sent to a Telegram chat.
type OutgoingMessage struct {
	ChatID    int64
	Text      string
	ReplyTo   int              // 0 means no reply
	MessageID int              // nonzero = edit existing message instead of sending new
	DraftID   string           // non-empty = send as draft (streaming preview, Bot API 9.3+)
	ResultCh  chan int          // if non-nil, the created/edited MessageID is sent back
	HTML      bool             // if true, convert markdown to Telegram HTML before sending
	Buttons   [][]InlineButton // inline keyboard rows (nil = no keyboard)
}

// ReactionEvent represents a reaction update from Telegram.
type ReactionEvent struct {
	ChatID    int64
	UserID    int64
	MessageID int
	Emoji     string // e.g. "👍", "👎"
}

// CallbackEvent represents a callback query from an inline keyboard button press.
type CallbackEvent struct {
	ID        string // callback query ID (for answering)
	ChatID    int64
	UserID    int64
	MessageID int
	Data      string // callback_data from the button
}

// Channel is the Telegram transport actor. It bridges the Telegram Bot API
// with the rest of the actor system through typed message channels.
type Channel struct {
	cfg     config.TGConfig
	allowed map[int64]struct{} // empty map means allow all

	inbox     chan OutgoingMessage // session -> telegram
	updates   chan IncomingMessage // telegram -> session
	typing    chan int64           // chat IDs to send typing action to
	docSend   chan docSendRequest  // document send requests
	reactions chan ReactionEvent   // reaction updates from telegram
	callbacks chan CallbackEvent   // callback query events from inline keyboards
}

// docSendRequest is an internal request to send a document.
type docSendRequest struct {
	ChatID   int64
	FileName string
	Data     []byte
	Caption  string
	ErrCh    chan error
}

// NewChannel creates a Channel actor from the given Telegram config.
// It validates that the token is non-empty but does not dial Telegram yet;
// the actual bot connection is established inside Run.
func NewChannel(cfg config.TGConfig) (*Channel, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}

	allowed := make(map[int64]struct{}, len(cfg.AllowedID))
	for _, id := range cfg.AllowedID {
		allowed[id] = struct{}{}
	}

	return &Channel{
		cfg:       cfg,
		allowed:   allowed,
		inbox:     make(chan OutgoingMessage, channelBuffer),
		updates:   make(chan IncomingMessage, channelBuffer),
		typing:    make(chan int64, channelBuffer),
		docSend:   make(chan docSendRequest, 8),
		reactions: make(chan ReactionEvent, channelBuffer),
		callbacks: make(chan CallbackEvent, channelBuffer),
	}, nil
}

// Name implements actor.Actor.
func (ch *Channel) Name() string { return "telegram" }

// Inbox returns a send-only channel that the session actor uses to push
// outgoing responses toward Telegram.
func (ch *Channel) Inbox() chan<- OutgoingMessage { return ch.inbox }

// Updates returns a receive-only channel that the session actor reads to
// get incoming user messages.
func (ch *Channel) Updates() <-chan IncomingMessage { return ch.updates }

// Reactions returns a receive-only channel for Telegram reaction events.
func (ch *Channel) Reactions() <-chan ReactionEvent { return ch.reactions }

// Callbacks returns a receive-only channel for inline keyboard callback events.
func (ch *Channel) Callbacks() <-chan CallbackEvent { return ch.callbacks }

// Run implements actor.Actor. It connects to the Telegram Bot API using the
// go-telegram/bot library with handler-based update routing. Incoming messages
// and reactions are dispatched via registered handlers. Outgoing messages are
// processed from the Inbox channel. Blocks until ctx is cancelled.
func (ch *Channel) Run(ctx context.Context) error {
	b, err := bot.New(ch.cfg.Token,
		bot.WithDefaultHandler(ch.defaultHandler),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMessageReaction,
			models.AllowedUpdateCallbackQuery,
		}),
	)
	if err != nil {
		return fmt.Errorf("telegram: create bot: %w", err)
	}

	me, err := b.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram: get bot info: %w", err)
	}
	slog.Info("telegram bot authorised", "username", me.Username)

	// Register slash commands so they show in Telegram's autocomplete menu.
	_, err = b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "effort", Description: "Show or set thinking effort level (low/medium/high/max)"},
			{Command: "retry", Description: "Replay last message at higher effort"},
			{Command: "debug", Description: "Toggle tool call visibility (on/off)"},
		},
	})
	if err != nil {
		slog.Warn("telegram: failed to register bot commands", "err", err)
	}

	// Start polling in a background goroutine. The library handles long polling internally.
	go b.Start(ctx)

	rateTick := time.NewTicker(sendInterval)
	defer rateTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg := <-ch.inbox:
			ch.sendMessage(ctx, b, msg, rateTick)

		case chatID := <-ch.typing:
			if _, err := b.SendChatAction(ctx, &bot.SendChatActionParams{
				ChatID: chatID,
				Action: models.ChatActionTyping,
			}); err != nil {
				slog.Debug("telegram: typing action failed", "chat_id", chatID, "err", err)
			}

		case req := <-ch.docSend:
			_, sendErr := b.SendDocument(ctx, &bot.SendDocumentParams{
				ChatID:  req.ChatID,
				Document: &models.InputFileUpload{Filename: req.FileName, Data: bytes.NewReader(req.Data)},
				Caption: req.Caption,
			})
			if req.ErrCh != nil {
				req.ErrCh <- sendErr
			}
		}
	}
}

// defaultHandler processes all incoming updates. It dispatches messages to the
// updates channel, reactions to the reactions channel, and callbacks to the callbacks channel.
func (ch *Channel) defaultHandler(ctx context.Context, b *bot.Bot, upd *models.Update) {
	if upd.CallbackQuery != nil {
		ch.handleCallback(ctx, b, upd.CallbackQuery)
		return
	}

	if upd.MessageReaction != nil {
		ch.handleReaction(upd.MessageReaction)
		return
	}

	if upd.Message == nil || upd.Message.From == nil {
		return
	}

	ch.handleMessage(ctx, b, upd.Message)
}

// handleMessage filters and forwards a single Telegram message.
func (ch *Channel) handleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	slog.Info("telegram: raw message received", "message_id", msg.ID, "chat_id", msg.Chat.ID)

	hasText := msg.Text != "" || msg.Caption != ""
	hasPhoto := len(msg.Photo) > 0
	hasDocument := msg.Document != nil
	hasVoice := msg.Voice != nil
	hasAudio := msg.Audio != nil
	if !hasText && !hasPhoto && !hasDocument && !hasVoice && !hasAudio {
		return
	}

	userID := msg.From.ID
	if !ch.isAllowed(userID) {
		slog.Warn("telegram: ignoring message from disallowed user", "user_id", userID)
		return
	}

	text := msg.Text
	if text == "" && msg.Caption != "" {
		text = msg.Caption
	}

	incoming := IncomingMessage{
		ChatID:    msg.Chat.ID,
		UserID:    userID,
		Text:      text,
		MessageID: msg.ID,
		ChatType:  string(msg.Chat.Type),
	}

	// Download attachments.
	if hasPhoto {
		photos := msg.Photo
		best := photos[len(photos)-1] // last = largest
		att, err := ch.downloadFile(ctx, b, best.FileID, AttachPhoto, "", maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download photo", "err", err)
		} else {
			incoming.Attachments = append(incoming.Attachments, att)
		}
	}
	if hasDocument {
		doc := msg.Document
		att, err := ch.downloadFile(ctx, b, doc.FileID, AttachDocument, doc.FileName, maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download document", "err", err)
		} else {
			if doc.MimeType != "" {
				att.MimeType = doc.MimeType
			}
			incoming.Attachments = append(incoming.Attachments, att)
		}
	}
	if hasVoice {
		v := msg.Voice
		att, err := ch.downloadFile(ctx, b, v.FileID, AttachVoice, "", maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download voice", "err", err)
		} else {
			att.Duration = v.Duration
			if v.MimeType != "" {
				att.MimeType = v.MimeType
			} else if att.MimeType == "" {
				att.MimeType = "audio/ogg"
			}
			incoming.Attachments = append(incoming.Attachments, att)
		}
	}
	if hasAudio {
		a := msg.Audio
		att, err := ch.downloadFile(ctx, b, a.FileID, AttachAudio, a.FileName, maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download audio", "err", err)
		} else {
			if a.MimeType != "" {
				att.MimeType = a.MimeType
			}
			att.Duration = a.Duration
			incoming.Attachments = append(incoming.Attachments, att)
		}
	}

	select {
	case ch.updates <- incoming:
	default:
		slog.Warn("telegram: updates channel full, dropping message",
			"chat_id", incoming.ChatID, "user_id", incoming.UserID)
	}
}

// handleReaction processes a Telegram reaction update and forwards it to the reactions channel.
func (ch *Channel) handleReaction(reaction *models.MessageReactionUpdated) {
	if reaction.User == nil {
		return
	}
	if !ch.isAllowed(reaction.User.ID) {
		return
	}

	// Extract the new emoji reaction (if any).
	for _, r := range reaction.NewReaction {
		if r.Type == models.ReactionTypeTypeEmoji && r.ReactionTypeEmoji != nil {
			select {
			case ch.reactions <- ReactionEvent{
				ChatID:    reaction.Chat.ID,
				UserID:    reaction.User.ID,
				MessageID: reaction.MessageID,
				Emoji:     r.ReactionTypeEmoji.Emoji,
			}:
			default:
				slog.Warn("telegram: reaction channel full, dropping", "chat_id", reaction.Chat.ID)
			}
		}
	}
}

// handleCallback processes a Telegram callback query from an inline keyboard button press.
func (ch *Channel) handleCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if !ch.isAllowed(cq.From.ID) {
		return
	}

	// Answer the callback to dismiss the loading indicator.
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID}) //nolint:errcheck

	msgID := 0
	chatID := int64(0)
	if cq.Message.Message != nil {
		msgID = cq.Message.Message.ID
		chatID = cq.Message.Message.Chat.ID
	}

	select {
	case ch.callbacks <- CallbackEvent{
		ID:        cq.ID,
		ChatID:    chatID,
		UserID:    cq.From.ID,
		MessageID: msgID,
		Data:      cq.Data,
	}:
	default:
		slog.Warn("telegram: callback channel full, dropping", "data", cq.Data)
	}
}

// downloadFile fetches any file from Telegram servers and returns it as an Attachment.
func (ch *Channel) downloadFile(ctx context.Context, b *bot.Bot, fileID string, kind AttachmentKind, fileName string, maxBytes int64) (Attachment, error) {
	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return Attachment{}, fmt.Errorf("get file: %w", err)
	}

	fileURL := b.FileDownloadLink(file)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fileURL) //nolint:gosec // URL from Telegram API
	if err != nil {
		return Attachment{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	// Read up to maxBytes+1 to detect truncation.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Attachment{}, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return Attachment{}, fmt.Errorf("file too large (>%d bytes)", maxBytes)
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" && kind == AttachPhoto {
		mime = "image/jpeg"
	}

	return Attachment{
		Kind:     kind,
		Data:     data,
		MimeType: mime,
		FileName: fileName,
		FileID:   fileID,
	}, nil
}

// SendTyping sends a "typing..." chat action indicator to the given chat.
// Safe to call from any goroutine; the action is executed in the Run loop.
func (ch *Channel) SendTyping(chatID int64) {
	select {
	case ch.typing <- chatID:
	default: // drop if buffer full
	}
}

// SendDocument sends a file/document to the given chat.
// Blocks until the send completes or fails.
func (ch *Channel) SendDocument(chatID int64, fileName string, data []byte, caption string) error {
	errCh := make(chan error, 1)
	ch.docSend <- docSendRequest{
		ChatID:   chatID,
		FileName: fileName,
		Data:     data,
		Caption:  caption,
		ErrCh:    errCh,
	}
	return <-errCh
}

// isAllowed reports whether the given user ID is permitted to interact
// with the bot. An empty allow-list means everyone is allowed.
func (ch *Channel) isAllowed(userID int64) bool {
	if len(ch.allowed) == 0 {
		return true
	}
	_, ok := ch.allowed[userID]
	return ok
}

// sendMessage sends or edits an OutgoingMessage. If MessageID is set, it
// edits the existing message instead of creating a new one. If ResultCh is
// set, it sends the message ID of the created/edited message back.
// For new messages that exceed 4096 runes, it splits into chunks.
func (ch *Channel) sendMessage(ctx context.Context, b *bot.Bot, msg OutgoingMessage, rateTick *time.Ticker) {
	// Draft message (streaming preview via Bot API 9.3+ sendMessageDraft).
	// Fire-and-forget: drafts show a typing preview that gets replaced by the final sendMessage.
	if msg.DraftID != "" {
		text := msg.Text
		if utf8.RuneCountInString(text) > maxMessageLen {
			r := []rune(text)
			text = string(r[:maxMessageLen-3]) + "..."
		}
		if _, err := b.SendMessageDraft(ctx, &bot.SendMessageDraftParams{
			ChatID:  msg.ChatID,
			DraftID: msg.DraftID,
			Text:    text,
		}); err != nil {
			slog.Debug("telegram: draft send failed", "chat_id", msg.ChatID, "err", err)
		}
		return
	}

	// Edit existing message (streaming update path).
	if msg.MessageID != 0 {
		// Telegram edit limit is 4096 runes; truncate if needed.
		text := msg.Text
		if utf8.RuneCountInString(text) > maxMessageLen {
			r := []rune(text)
			text = string(r[:maxMessageLen-3]) + "..."
		}
		params := &bot.EditMessageTextParams{
			ChatID:    msg.ChatID,
			MessageID: msg.MessageID,
			Text:      text,
		}
		if msg.HTML {
			params.Text = mdhtml.ConvertSafe(text)
			params.ParseMode = models.ParseModeHTML
		}
		if _, err := b.EditMessageText(ctx, params); err != nil {
			// Retry without parse mode if Telegram can't parse the HTML.
			if msg.HTML && strings.Contains(err.Error(), "can't parse entities") {
				slog.Warn("telegram: HTML parse failed, retrying as plain text",
					"chat_id", msg.ChatID,
					"message_id", msg.MessageID,
				)
				params.Text = msg.Text
				params.ParseMode = ""
				if _, retryErr := b.EditMessageText(ctx, params); retryErr != nil {
					slog.Warn("telegram: failed to edit message (plain retry)",
						"chat_id", msg.ChatID,
						"message_id", msg.MessageID,
						"err", retryErr,
					)
				}
			} else if strings.Contains(err.Error(), "Too Many Requests") && msg.HTML {
				// Rate-limited on the final HTML edit. Wait and retry once
				// so the user sees formatted text instead of raw markdown.
				slog.Warn("telegram: rate limited on HTML edit, will retry",
					"chat_id", msg.ChatID,
					"message_id", msg.MessageID,
				)
				time.Sleep(3 * time.Second)
				if _, retryErr := b.EditMessageText(ctx, params); retryErr != nil {
					slog.Warn("telegram: failed to edit message (rate limit retry)",
						"chat_id", msg.ChatID,
						"message_id", msg.MessageID,
						"err", retryErr,
					)
				}
			} else {
				slog.Warn("telegram: failed to edit message",
					"chat_id", msg.ChatID,
					"message_id", msg.MessageID,
					"err", err,
				)
			}
		}
		if msg.ResultCh != nil {
			msg.ResultCh <- msg.MessageID
		}
		return
	}

	// New message path (with chunking for long messages).
	chunks := chunkMessage(msg.Text)

	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID: msg.ChatID,
			Text:   chunk,
		}
		if msg.HTML {
			params.Text = mdhtml.ConvertSafe(chunk)
			params.ParseMode = models.ParseModeHTML
		}

		// Only the first chunk replies to the original message.
		if i == 0 && msg.ReplyTo != 0 {
			params.ReplyParameters = &models.ReplyParameters{
				MessageID: msg.ReplyTo,
			}
		}

		// Attach inline keyboard to the last chunk only.
		if i == len(chunks)-1 && len(msg.Buttons) > 0 {
			params.ReplyMarkup = buildInlineKeyboard(msg.Buttons)
		}

		sent, err := b.SendMessage(ctx, params)
		if err != nil {
			// Retry without parse mode if Telegram can't parse the HTML.
			if msg.HTML && strings.Contains(err.Error(), "can't parse entities") {
				slog.Warn("telegram: HTML parse failed, retrying as plain text",
					"chat_id", msg.ChatID,
					"chunk", i+1,
				)
				params.Text = chunk
				params.ParseMode = ""
				sent, err = b.SendMessage(ctx, params)
			}
			if err != nil {
				slog.Error("telegram: failed to send message",
					"chat_id", msg.ChatID,
					"chunk", i+1,
					"err", err,
				)
				if msg.ResultCh != nil {
					msg.ResultCh <- 0
				}
				return
			}
		}

		// Send back the message ID of the first chunk (for streaming).
		if i == 0 && msg.ResultCh != nil {
			msg.ResultCh <- sent.ID
		}

		// Wait for the rate-limit ticker before sending the next chunk.
		if i < len(chunks)-1 {
			<-rateTick.C
		}
	}
}

// ---------------------------------------------------------------------------
// Message chunking
// ---------------------------------------------------------------------------

// chunkMessage splits text into pieces that each fit within maxMessageLen.
// It tries to split on paragraph boundaries (\n\n), avoids breaking inside
// code fences (``` blocks), and falls back to newline splits for oversized
// single paragraphs.
func chunkMessage(text string) []string {
	if utf8.RuneCountInString(text) <= maxMessageLen {
		return []string{text}
	}

	paragraphs := splitParagraphs(text)
	var chunks []string
	var buf strings.Builder
	bufRunes := 0

	for _, para := range paragraphs {
		// If adding this paragraph would exceed the limit, flush the
		// buffer first.
		paraRunes := utf8.RuneCountInString(para)
		neededRunes := paraRunes
		if bufRunes > 0 {
			neededRunes += 2 // "\n\n"
		}

		if bufRunes+neededRunes > maxMessageLen {
			// Flush whatever we have accumulated.
			if bufRunes > 0 {
				chunks = append(chunks, buf.String())
				buf.Reset()
				bufRunes = 0
			}

			// If the paragraph itself is too long, split it further.
			if paraRunes > maxMessageLen {
				sub := splitLongParagraph(para)
				chunks = append(chunks, sub...)
				continue
			}
		}

		if bufRunes > 0 {
			buf.WriteString("\n\n")
			bufRunes += 2
		}
		buf.WriteString(para)
		bufRunes += paraRunes
	}

	if bufRunes > 0 {
		chunks = append(chunks, buf.String())
	}
	return chunks
}

// splitParagraphs splits text on double-newline boundaries but never breaks
// inside a fenced code block (``` ... ```).
func splitParagraphs(text string) []string {
	var paragraphs []string
	var buf strings.Builder
	inFence := false

	lines := strings.Split(text, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track code-fence state.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}

		isBlank := trimmed == ""

		// A paragraph break is a blank line while outside a code fence.
		if isBlank && !inFence && buf.Len() > 0 {
			paragraphs = append(paragraphs, strings.TrimRight(buf.String(), "\n"))
			buf.Reset()
			continue
		}

		// Skip consecutive blank lines between paragraphs.
		if isBlank && buf.Len() == 0 {
			continue
		}

		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
	}

	if buf.Len() > 0 {
		paragraphs = append(paragraphs, strings.TrimRight(buf.String(), "\n"))
	}
	return paragraphs
}

// splitLongParagraph breaks a single oversized paragraph at the nearest
// newline that keeps each piece under maxMessageLen runes. As a last resort it
// hard-cuts at maxMessageLen runes.
func splitLongParagraph(text string) []string {
	var chunks []string

	for utf8.RuneCountInString(text) > maxMessageLen {
		// Find the byte offset corresponding to maxMessageLen runes.
		byteLimit := runeOffsetToByteOffset(text, maxMessageLen)
		// Find the last newline within the rune limit.
		cut := strings.LastIndex(text[:byteLimit], "\n")
		if cut <= 0 {
			// No newline found — hard cut at the rune boundary.
			cut = byteLimit
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
		// Strip a leading newline from the remainder so the next chunk
		// doesn't start with a blank line.
		text = strings.TrimPrefix(text, "\n")
	}

	if len(text) > 0 {
		chunks = append(chunks, text)
	}
	return chunks
}

// runeOffsetToByteOffset returns the byte position of the n-th rune in s.
func runeOffsetToByteOffset(s string, n int) int {
	offset := 0
	for i := 0; i < n && offset < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[offset:])
		offset += size
	}
	return offset
}

// buildInlineKeyboard converts rows of InlineButton to a Telegram InlineKeyboardMarkup.
func buildInlineKeyboard(rows [][]InlineButton) *models.InlineKeyboardMarkup {
	var keyboard [][]models.InlineKeyboardButton
	for _, row := range rows {
		var kbRow []models.InlineKeyboardButton
		for _, btn := range row {
			kbRow = append(kbRow, models.InlineKeyboardButton{
				Text:         btn.Text,
				CallbackData: btn.CallbackData,
			})
		}
		keyboard = append(keyboard, kbRow)
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: keyboard}
}
