package tgtest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

const (
	// replyPoll is how often the chat is re-read while waiting for the bot.
	replyPoll = time.Second
	// historyLimit is how many recent messages are inspected per poll.
	historyLimit = 20
)

// User is a real Telegram account driving the bot under test.
type User struct {
	api    *tg.Client
	sender *message.Sender
	bot    tg.InputPeerClass
	self   *tg.User
}

// Run signs in with the stored session and hands a User to fn.
//
// It refuses to start without an authorised session: creating one needs a code
// only a human can read, so the tests must never appear to hang on it.
func Run(ctx context.Context, cfg Config, botUsername string, fn func(context.Context, *User) error) error {
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: cfg.SessionStorage(),
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			return fmt.Errorf("no authorised session in %s; run: go run ./test/e2e/login", cfg.SessionFile)
		}

		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}

		api := client.API()

		peer, err := resolveBot(ctx, api, botUsername)
		if err != nil {
			return err
		}

		return fn(ctx, &User{
			api:    api,
			sender: message.NewSender(api),
			bot:    peer,
			self:   self,
		})
	})
}

// SelfID is the spare account's id, which is also the chat the bot answers in.
func (u *User) SelfID() int64 {
	return u.self.ID
}

// Send delivers a message to the bot as the user would type it.
func (u *User) Send(ctx context.Context, text string) error {
	_, err := u.sender.To(u.bot).Text(ctx, text)
	if err != nil {
		return fmt.Errorf("send %q: %w", text, err)
	}

	return nil
}

// WaitForReply blocks until the bot sends a message containing want, and
// returns it. The whole point of these tests is what the chat ends up showing,
// so matching is on the visible text.
func (u *User) WaitForReply(ctx context.Context, want string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	seen := map[int]bool{}

	// Ignore anything already in the chat, so a previous run's reply cannot
	// satisfy this wait.
	before, err := u.incoming(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range before {
		seen[m.ID] = true
	}

	for {
		messages, err := u.incoming(ctx)
		if err != nil {
			return "", err
		}

		for _, m := range messages {
			if seen[m.ID] {
				continue
			}
			if strings.Contains(m.Message, want) {
				return m.Message, nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("no reply containing %q within %s (last: %q)", want, timeout, lastText(messages))
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(replyPoll):
		}
	}
}

// ExpectSilence fails if the bot answers at all within the window, which is how
// "commands from other chats are ignored" is checked.
func (u *User) ExpectSilence(ctx context.Context, window time.Duration) error {
	before, err := u.incoming(ctx)
	if err != nil {
		return err
	}
	baseline := len(before)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(window):
	}

	after, err := u.incoming(ctx)
	if err != nil {
		return err
	}

	if len(after) > baseline {
		return fmt.Errorf("expected no reply, got %q", lastText(after))
	}

	return nil
}

// SentMessageExists reports whether a message the user sent is still in the
// chat, used to check that the bot deleted a credential.
func (u *User) SentMessageExists(ctx context.Context, text string) (bool, error) {
	messages, err := u.history(ctx)
	if err != nil {
		return false, err
	}

	for _, m := range messages {
		if m.Out && m.Message == text {
			return true, nil
		}
	}

	return false, nil
}

// incoming returns messages sent by the bot, newest first.
func (u *User) incoming(ctx context.Context) ([]*tg.Message, error) {
	messages, err := u.history(ctx)
	if err != nil {
		return nil, err
	}

	var out []*tg.Message
	for _, m := range messages {
		if !m.Out {
			out = append(out, m)
		}
	}

	return out, nil
}

func (u *User) history(ctx context.Context) ([]*tg.Message, error) {
	res, err := u.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  u.bot,
		Limit: historyLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}

	modified, ok := res.AsModified()
	if !ok {
		return nil, fmt.Errorf("unexpected history response %T", res)
	}

	var out []*tg.Message
	for _, m := range modified.GetMessages() {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		out = append(out, msg)
	}

	return out, nil
}

func resolveBot(ctx context.Context, api *tg.Client, username string) (tg.InputPeerClass, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: strings.TrimPrefix(username, "@"),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve @%s: %w", username, err)
	}

	for _, user := range resolved.Users {
		u, ok := user.(*tg.User)
		if !ok {
			continue
		}
		return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}, nil
	}

	return nil, fmt.Errorf("@%s did not resolve to a user", username)
}

func lastText(messages []*tg.Message) string {
	if len(messages) == 0 {
		return ""
	}

	return messages[0].Message
}
