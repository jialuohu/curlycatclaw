package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

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

// OutgoingMessage represents a message to be sent to a Telegram chat.
type OutgoingMessage struct {
	ChatID    int64
	Text      string
	ReplyTo   int      // 0 means no reply
	MessageID int      // nonzero = edit existing message instead of sending new
	ResultCh  chan int  // if non-nil, the created/edited MessageID is sent back
	HTML      bool     // if true, convert markdown to Telegram HTML before sending
}

// Channel is the Telegram transport actor. It bridges the Telegram Bot API
// with the rest of the actor system through typed message channels.
type Channel struct {
	cfg     config.TGConfig
	allowed map[int64]struct{} // empty map means allow all

	inbox   chan OutgoingMessage  // session -> telegram
	updates chan IncomingMessage  // telegram -> session
	typing  chan int64            // chat IDs to send typing action to
	docSend chan docSendRequest   // document send requests
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
		cfg:     cfg,
		allowed: allowed,
		inbox:   make(chan OutgoingMessage, channelBuffer),
		updates: make(chan IncomingMessage, channelBuffer),
		typing:  make(chan int64, channelBuffer),
		docSend: make(chan docSendRequest, 8),
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

// Run implements actor.Actor. It connects to the Telegram Bot API, fans
// incoming updates into the Updates channel, and sends outgoing messages
// from the Inbox channel. It blocks until ctx is cancelled.
func (ch *Channel) Run(ctx context.Context) error {
	bot, err := tgbotapi.NewBotAPI(ch.cfg.Token)
	if err != nil {
		return fmt.Errorf("telegram: create bot: %w", err)
	}

	slog.Info("telegram bot authorised", "username", bot.Self.UserName)

	// Register slash commands so they show in Telegram's autocomplete menu.
	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "effort", Description: "Show or set thinking effort level (low/medium/high/max)"},
		tgbotapi.BotCommand{Command: "retry", Description: "Replay last message at higher effort"},
		tgbotapi.BotCommand{Command: "project", Description: "Switch active project context"},
	)
	if _, err := bot.Request(commands); err != nil {
		slog.Warn("telegram: failed to register bot commands", "err", err)
	}

	ucfg := tgbotapi.NewUpdate(0)
	ucfg.Timeout = 30
	tgUpdates := bot.GetUpdatesChan(ucfg)

	rateTick := time.NewTicker(sendInterval)
	defer rateTick.Stop()

	for {
		select {
		case <-ctx.Done():
			bot.StopReceivingUpdates()
			// Do not close(ch.updates) here — the session actor's select
			// may still be reading from it, and closing would race with
			// sends from handleUpdate. The GC will collect the channel
			// once all references are dropped.
			return ctx.Err()

		case upd, ok := <-tgUpdates:
			if !ok {
				return fmt.Errorf("telegram: update channel closed")
			}
			ch.handleUpdate(upd, bot)

		case msg := <-ch.inbox:
			ch.sendMessage(bot, msg, rateTick)

		case chatID := <-ch.typing:
			action := tgbotapi.NewChatAction(chatID, "typing")
			if _, err := bot.Send(action); err != nil {
				slog.Debug("telegram: typing action failed", "chat_id", chatID, "err", err)
			}

		case req := <-ch.docSend:
			doc := tgbotapi.NewDocument(req.ChatID, tgbotapi.FileBytes{Name: req.FileName, Bytes: req.Data})
			if req.Caption != "" {
				doc.Caption = req.Caption
			}
			_, err := bot.Send(doc)
			if req.ErrCh != nil {
				req.ErrCh <- err
			}
		}
	}
}

// handleUpdate filters and forwards a single Telegram update.
func (ch *Channel) handleUpdate(upd tgbotapi.Update, bot *tgbotapi.BotAPI) {
	slog.Info("telegram: raw update received", "update_id", upd.UpdateID, "has_message", upd.Message != nil)
	if upd.Message == nil || upd.Message.From == nil {
		return
	}

	// Accept messages with text, photos, documents, voice, or combinations.
	hasText := upd.Message.Text != "" || upd.Message.Caption != ""
	hasPhoto := len(upd.Message.Photo) > 0
	hasDocument := upd.Message.Document != nil
	hasVoice := upd.Message.Voice != nil
	hasAudio := upd.Message.Audio != nil
	if !hasText && !hasPhoto && !hasDocument && !hasVoice && !hasAudio {
		return
	}

	userID := upd.Message.From.ID
	if !ch.isAllowed(userID) {
		slog.Warn("telegram: ignoring message from disallowed user",
			"user_id", userID,
		)
		return
	}

	text := upd.Message.Text
	if text == "" && upd.Message.Caption != "" {
		text = upd.Message.Caption
	}

	msg := IncomingMessage{
		ChatID:    upd.Message.Chat.ID,
		UserID:    userID,
		Text:      text,
		MessageID: upd.Message.MessageID,
		ChatType:  upd.Message.Chat.Type,
	}

	// Download attachments.
	if hasPhoto {
		photos := upd.Message.Photo
		best := photos[len(photos)-1] // last = largest
		att, err := ch.downloadFile(bot, best.FileID, AttachPhoto, "", maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download photo", "err", err)
		} else {
			msg.Attachments = append(msg.Attachments, att)
		}
	}
	if hasDocument {
		doc := upd.Message.Document
		att, err := ch.downloadFile(bot, doc.FileID, AttachDocument, doc.FileName, maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download document", "err", err)
		} else {
			if doc.MimeType != "" {
				att.MimeType = doc.MimeType
			}
			msg.Attachments = append(msg.Attachments, att)
		}
	}
	if hasVoice {
		v := upd.Message.Voice
		att, err := ch.downloadFile(bot, v.FileID, AttachVoice, "", maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download voice", "err", err)
		} else {
			att.Duration = v.Duration
			if v.MimeType != "" {
				att.MimeType = v.MimeType
			} else if att.MimeType == "" {
				att.MimeType = "audio/ogg"
			}
			msg.Attachments = append(msg.Attachments, att)
		}
	}
	if hasAudio {
		a := upd.Message.Audio
		att, err := ch.downloadFile(bot, a.FileID, AttachAudio, a.FileName, maxDownloadBytes)
		if err != nil {
			slog.Warn("telegram: failed to download audio", "err", err)
		} else {
			if a.MimeType != "" {
				att.MimeType = a.MimeType
			}
			att.Duration = a.Duration
			msg.Attachments = append(msg.Attachments, att)
		}
	}

	ch.updates <- msg
}

// downloadFile fetches any file from Telegram servers and returns it as an Attachment.
func (ch *Channel) downloadFile(bot *tgbotapi.BotAPI, fileID string, kind AttachmentKind, fileName string, maxBytes int64) (Attachment, error) {
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return Attachment{}, fmt.Errorf("get file: %w", err)
	}

	fileURL := file.Link(bot.Token)
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
func (ch *Channel) sendMessage(bot *tgbotapi.BotAPI, msg OutgoingMessage, rateTick *time.Ticker) {
	// Edit existing message (streaming update path).
	if msg.MessageID != 0 {
		// Telegram edit limit is 4096 runes; truncate if needed.
		text := msg.Text
		if utf8.RuneCountInString(text) > maxMessageLen {
			r := []rune(text)
			text = string(r[:maxMessageLen-3]) + "..."
		}
		edit := tgbotapi.NewEditMessageText(msg.ChatID, msg.MessageID, text)
		if msg.HTML {
			edit.Text = mdhtml.ConvertSafe(text)
			edit.ParseMode = tgbotapi.ModeHTML
		}
		if _, err := bot.Send(edit); err != nil {
			// Retry without parse mode if Telegram can't parse the HTML.
			if msg.HTML && strings.Contains(err.Error(), "can't parse entities") {
				slog.Warn("telegram: HTML parse failed, retrying as plain text",
					"chat_id", msg.ChatID,
					"message_id", msg.MessageID,
				)
				edit.Text = msg.Text
				edit.ParseMode = ""
				if _, retryErr := bot.Send(edit); retryErr != nil {
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
				if _, retryErr := bot.Send(edit); retryErr != nil {
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
		mc := tgbotapi.NewMessage(msg.ChatID, chunk)
		if msg.HTML {
			mc.Text = mdhtml.ConvertSafe(chunk)
			mc.ParseMode = tgbotapi.ModeHTML
		}

		// Only the first chunk replies to the original message.
		if i == 0 && msg.ReplyTo != 0 {
			mc.ReplyToMessageID = msg.ReplyTo
		}

		sent, err := bot.Send(mc)
		if err != nil {
			// Retry without parse mode if Telegram can't parse the HTML.
			if msg.HTML && strings.Contains(err.Error(), "can't parse entities") {
				slog.Warn("telegram: HTML parse failed, retrying as plain text",
					"chat_id", msg.ChatID,
					"chunk", i+1,
				)
				mc.Text = chunk
				mc.ParseMode = ""
				sent, err = bot.Send(mc)
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
			msg.ResultCh <- sent.MessageID
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
