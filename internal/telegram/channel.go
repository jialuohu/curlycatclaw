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
)

const (
	// maxMessageLen is Telegram's hard limit per message.
	maxMessageLen = 4096

	// sendInterval is the minimum delay between outgoing messages to stay
	// well under Telegram's ~30 msgs/sec rate limit.
	sendInterval = 50 * time.Millisecond

	// channelBuffer is the capacity for both the inbox and updates channels.
	channelBuffer = 64
)

// IncomingMessage represents a message received from a Telegram user.
// It may contain text, photos, or both (photo with caption).
type IncomingMessage struct {
	ChatID    int64
	UserID    int64
	Text      string
	MessageID int
	// Photos contains base64-encoded image data from attached photos.
	// The channel actor downloads the best-quality photo from Telegram.
	Photos []Photo
}

// Photo represents an image attached to a message.
type Photo struct {
	Data      []byte // raw image bytes (downloaded from Telegram)
	MimeType  string // e.g. "image/jpeg"
	FileID    string // Telegram file_id for reference storage
	Width     int
	Height    int
}

// OutgoingMessage represents a message to be sent to a Telegram chat.
type OutgoingMessage struct {
	ChatID    int64
	Text      string
	ReplyTo   int      // 0 means no reply
	MessageID int      // nonzero = edit existing message instead of sending new
	ResultCh  chan int  // if non-nil, the created/edited MessageID is sent back
}

// Channel is the Telegram transport actor. It bridges the Telegram Bot API
// with the rest of the actor system through typed message channels.
type Channel struct {
	cfg     config.TGConfig
	allowed map[int64]struct{} // empty map means allow all

	inbox   chan OutgoingMessage  // session -> telegram
	updates chan IncomingMessage  // telegram -> session
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
		}
	}
}

// handleUpdate filters and forwards a single Telegram update.
func (ch *Channel) handleUpdate(upd tgbotapi.Update, bot *tgbotapi.BotAPI) {
	if upd.Message == nil || upd.Message.From == nil {
		return
	}

	// Accept messages with text, photos, or both (caption).
	hasText := upd.Message.Text != "" || upd.Message.Caption != ""
	hasPhoto := upd.Message.Photo != nil && len(upd.Message.Photo) > 0
	if !hasText && !hasPhoto {
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
	}

	// Download photos if present (use the largest available size).
	if hasPhoto {
		photos := upd.Message.Photo
		best := photos[len(photos)-1] // last = largest
		photo, err := ch.downloadPhoto(bot, best)
		if err != nil {
			slog.Warn("telegram: failed to download photo", "err", err)
		} else {
			msg.Photos = append(msg.Photos, photo)
		}
	}

	ch.updates <- msg
}

// downloadPhoto fetches photo data from Telegram servers.
func (ch *Channel) downloadPhoto(bot *tgbotapi.BotAPI, ps tgbotapi.PhotoSize) (Photo, error) {
	fileConfig := tgbotapi.FileConfig{FileID: ps.FileID}
	file, err := bot.GetFile(fileConfig)
	if err != nil {
		return Photo{}, fmt.Errorf("get file: %w", err)
	}

	fileURL := file.Link(bot.Token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fileURL) //nolint:gosec // URL from Telegram API
	if err != nil {
		return Photo{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB max
	if err != nil {
		return Photo{}, fmt.Errorf("read body: %w", err)
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}

	return Photo{
		Data:     data,
		MimeType: mime,
		FileID:   ps.FileID,
		Width:    ps.Width,
		Height:   ps.Height,
	}, nil
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
// For new messages that exceed 4096 chars, it splits into chunks.
func (ch *Channel) sendMessage(bot *tgbotapi.BotAPI, msg OutgoingMessage, rateTick *time.Ticker) {
	// Edit existing message (streaming update path).
	if msg.MessageID != 0 {
		// Telegram edit limit is 4096 chars; truncate if needed.
		text := msg.Text
		if utf8.RuneCountInString(text) > maxMessageLen {
			r := []rune(text)
			text = string(r[:maxMessageLen-3]) + "..."
		}
		edit := tgbotapi.NewEditMessageText(msg.ChatID, msg.MessageID, text)
		if _, err := bot.Send(edit); err != nil {
			slog.Warn("telegram: failed to edit message",
				"chat_id", msg.ChatID,
				"message_id", msg.MessageID,
				"err", err,
			)
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

		// Only the first chunk replies to the original message.
		if i == 0 && msg.ReplyTo != 0 {
			mc.ReplyToMessageID = msg.ReplyTo
		}

		sent, err := bot.Send(mc)
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
