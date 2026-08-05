package notifier

import (
	"context"
	"fmt"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"

	"github.com/arvlas/instalker/internal/domain"
)

const (
	// telegramCaptionLimit is the maximum caption length for a media message.
	telegramCaptionLimit = 1024
	// albumLimit is the maximum number of items Telegram accepts in one album.
	albumLimit = 10
	// pollerTimeout keeps telebot from starting its own long-polling loop; the
	// bot only sends.
	pollerTimeout = 10 * time.Second
)

// Telegram delivers media to a single chat.
type Telegram struct {
	bot  *tele.Bot
	chat *tele.Chat
}

// NewTelegram builds a send-only Telegram client for the given chat.
func NewTelegram(token string, chatID int64) (*Telegram, error) {
	bot, err := tele.NewBot(tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: pollerTimeout},
	})
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	return &Telegram{bot: bot, chat: &tele.Chat{ID: chatID}}, nil
}

// Send delivers one Instagram media item, falling back to a text message when
// the files cannot be attached.
func (t *Telegram) Send(ctx context.Context, media domain.Media) error {
	caption := buildCaption(media)

	if len(media.Attachments) == 0 {
		_, err := t.bot.Send(t.chat, caption, tele.ModeHTML, tele.NoPreview)
		if err != nil {
			return fmt.Errorf("send text: %w", err)
		}
		return nil
	}

	album := make(tele.Album, 0, albumLimit)
	for i, att := range media.Attachments {
		if i == albumLimit {
			break
		}

		var item tele.Inputtable
		if att.IsVideo {
			item = &tele.Video{File: tele.FromURL(att.URL)}
		} else {
			item = &tele.Photo{File: tele.FromURL(att.URL)}
		}
		album = append(album, item)
	}

	// Telegram shows a single caption per album, carried by the first item.
	switch first := album[0].(type) {
	case *tele.Photo:
		first.Caption = caption
	case *tele.Video:
		first.Caption = caption
	}

	_, err := t.bot.SendAlbum(t.chat, album, tele.ModeHTML)
	if err == nil {
		return nil
	}

	// Instagram CDN links occasionally fail Telegram's server-side fetch; a
	// text message with the links is better than losing the notification.
	_, fallbackErr := t.bot.Send(t.chat, caption+"\n\n"+linkList(media), tele.ModeHTML, tele.NoPreview)
	if fallbackErr != nil {
		return fmt.Errorf("send album: %w (fallback: %v)", err, fallbackErr)
	}

	return nil
}

// Notify sends a plain service message, used for startup and error reporting.
func (t *Telegram) Notify(ctx context.Context, text string) error {
	_, err := t.bot.Send(t.chat, text, tele.ModeHTML, tele.NoPreview)
	if err != nil {
		return fmt.Errorf("send notice: %w", err)
	}

	return nil
}

func buildCaption(media domain.Media) string {
	var b strings.Builder

	label := "📸 New post"
	if media.Kind == domain.KindStory {
		label = "👻 New story"
	}

	fmt.Fprintf(&b, "%s from <b>%s</b>", label, escape(media.Owner.Username))
	if media.Owner.FullName != "" {
		fmt.Fprintf(&b, " (%s)", escape(media.Owner.FullName))
	}
	fmt.Fprintf(&b, "\n🕒 %s", media.TakenAt.Format(time.RFC1123))

	if media.Caption != "" {
		fmt.Fprintf(&b, "\n\n%s", escape(media.Caption))
	}
	if media.Permalink != "" {
		fmt.Fprintf(&b, "\n\n%s", media.Permalink)
	}

	return truncateCaption(b.String())
}

func linkList(media domain.Media) string {
	var b strings.Builder
	for i, att := range media.Attachments {
		fmt.Fprintf(&b, "<a href=\"%s\">file %d</a>\n", att.URL, i+1)
	}

	return b.String()
}

// truncateCaption trims to Telegram's caption limit without splitting a rune.
func truncateCaption(s string) string {
	if len(s) <= telegramCaptionLimit {
		return s
	}

	runes := []rune(s)
	for len(string(runes)) > telegramCaptionLimit-1 {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
