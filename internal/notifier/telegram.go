package notifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	tele "gopkg.in/telebot.v4"

	"github.com/arvlas/instalker/internal/domain"
)

const (
	// telegramCaptionLimit is the maximum caption length for a media message.
	telegramCaptionLimit = 1024
	// albumLimit is the maximum number of items Telegram accepts in one album.
	albumLimit = 10
	// pollerTimeout is how long each getUpdates call waits for a command.
	pollerTimeout = 10 * time.Second
	// probeTimeout bounds a /ping check so a throttled Instagram cannot leave
	// the command hanging indefinitely.
	probeTimeout = 60 * time.Second
)

// Telegram delivers media to a single chat.
type Telegram struct {
	bot  *tele.Bot
	chat *tele.Chat
}

// NewTelegram builds a Telegram client for the given chat. apiURL overrides the
// Bot API endpoint; empty means the real one.
func NewTelegram(token string, chatID int64, apiURL string) (*Telegram, error) {
	settings := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: pollerTimeout},
	}
	if apiURL != "" {
		settings.URL = apiURL
	}

	bot, err := tele.NewBot(settings)
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

// HandlePing wires /ping to a live scrape check. Commands from any other chat
// are ignored: the bot's username is public, so anyone can message it.
func (t *Telegram) HandlePing(ctx context.Context, probe func(context.Context) (domain.Probe, error)) {
	t.bot.Handle("/ping", func(c tele.Context) error {
		if c.Chat().ID != t.chat.ID {
			log.Warn().Int64("chat_id", c.Chat().ID).Msg("ignoring command from an unknown chat")
			return nil
		}

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		result, err := probe(probeCtx)
		if err != nil {
			// Reachable before polling has started, which is when this command
			// is most worth answering.
			log.Warn().Err(err).Msg("ping while not polling")
			return c.Send("🔴 <b>Not polling yet</b>\n"+escape(describeErr(err)), tele.ModeHTML, tele.NoPreview)
		}

		log.Info().Bool("ok", result.OK()).Dur("elapsed", result.Elapsed).Msg("ping probe finished")

		return c.Send(formatProbe(result), tele.ModeHTML, tele.NoPreview)
	})
}

// HandleSession wires /session to a session replacement, so a rotated cookie
// can be supplied from the chat instead of through a redeploy.
func (t *Telegram) HandleSession(ctx context.Context, update func(context.Context, string) error) {
	t.bot.Handle("/session", func(c tele.Context) error {
		if c.Chat().ID != t.chat.ID {
			log.Warn().Int64("chat_id", c.Chat().ID).Msg("ignoring command from an unknown chat")
			return nil
		}

		// The message carries a live credential; remove it from the chat
		// history regardless of whether it turns out to be valid.
		defer func() {
			err := t.bot.Delete(c.Message())
			if err != nil {
				log.Warn().Err(err).Msg("could not delete the message carrying the session cookie")
			}
		}()

		value := strings.TrimSpace(c.Message().Payload)
		if value == "" {
			return c.Send("Send the cookie with the command: <code>/session &lt;sessionid&gt;</code>", tele.ModeHTML)
		}

		err := update(ctx, value)
		if err != nil {
			// The error can quote the value, so it is never echoed back.
			log.Error().Err(err).Msg("failed to update the instagram session")
			if errors.Is(err, domain.ErrUnauthorized) {
				return c.Send("🔴 That does not look like a session cookie. Copy the <code>sessionid</code> value from instagram.com.", tele.ModeHTML)
			}
			return c.Send("🔴 Could not store the session, check the logs.", tele.ModeHTML)
		}

		log.Info().Msg("instagram session replaced from chat")

		return c.Send("✅ Session updated and saved. Run /ping to check it works.", tele.ModeHTML)
	})
}

// Run consumes Telegram updates until ctx is cancelled. Sending works without
// it; only commands need this loop.
func (t *Telegram) Run(ctx context.Context) error {
	err := t.bot.SetCommands([]tele.Command{
		{Text: "ping", Description: "check that Instagram scraping works"},
		{Text: "session", Description: "replace the Instagram session cookie"},
	})
	if err != nil {
		log.Warn().Err(err).Msg("failed to publish bot commands")
	}

	go func() {
		<-ctx.Done()
		t.bot.Stop()
	}()

	log.Info().Msg("telegram command listener started")
	t.bot.Start()
	log.Info().Msg("telegram command listener stopped")

	return nil
}

func formatProbe(probe domain.Probe) string {
	var b strings.Builder

	header := "🏓 <b>Instagram scraping is working</b>"
	if !probe.OK() {
		header = "🔴 <b>Instagram scraping is failing</b>"
	}
	fmt.Fprintf(&b, "%s\n<i>checked in %s</i>\n", header, probe.Elapsed.Round(time.Millisecond))

	if len(probe.Targets) == 0 {
		b.WriteString("\nNo targets are being watched.")
		return b.String()
	}

	for _, target := range probe.Targets {
		if !target.OK() {
			fmt.Fprintf(&b, "\n🔴 <b>%s</b> — %s", escape(target.User.Username), escape(describeErr(target.Err)))
			continue
		}

		fmt.Fprintf(&b, "\n✅ <b>%s</b> — %d posts, %d stories", escape(target.User.Username), target.Posts, target.Stories)
		if !target.Latest.IsZero() {
			fmt.Fprintf(&b, ", latest %s ago", time.Since(target.Latest).Round(time.Minute))
		}
	}

	return b.String()
}

// describeErr turns a scrape failure into something actionable, since the raw
// error is mostly Instagram's own noise.
func describeErr(err error) string {
	switch {
	case errors.Is(err, domain.ErrRateLimited):
		return "rate limited by Instagram (this host is being throttled)"
	case errors.Is(err, domain.ErrCheckpointRequired):
		return "Instagram wants a login challenge cleared"
	case errors.Is(err, domain.ErrUnauthorized):
		return "session rejected, send a fresh one with /session"
	case errors.Is(err, domain.ErrNotFound):
		return "account not found"
	default:
		return err.Error()
	}
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
