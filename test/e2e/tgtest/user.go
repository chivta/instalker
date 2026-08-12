package tgtest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	// replyPoll is how often the chat is re-read while waiting for the bot.
	// Polling faster trips Telegram's flood control for no benefit.
	replyPoll = 3 * time.Second
	// floodRetries bounds how many times a FLOOD_WAIT is waited out.
	floodRetries = 3
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

// Ask sends a command and waits for the reply it provokes.
//
// The chat is snapshotted before sending, not after: the bot often answers in
// under a second, and a snapshot taken afterwards would file that reply under
// "already there" and then wait for it forever.
func (u *User) Ask(ctx context.Context, command, want string, timeout time.Duration) (string, error) {
	before, err := u.incoming(ctx)
	if err != nil {
		return "", err
	}

	seen := make(map[int]bool, len(before))
	for _, m := range before {
		seen[m.ID] = true
	}

	err = u.Send(ctx, command)
	if err != nil {
		return "", err
	}

	return u.waitForReply(ctx, seen, want, timeout)
}

// waitForReply blocks until the bot sends a message containing want. The whole
// point of these tests is what the chat ends up showing, so matching is on the
// visible text.
func (u *User) waitForReply(ctx context.Context, seen map[int]bool, want string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)

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

// Silence watches for a reply that should never come, which is how "commands
// from other chats are ignored" is checked.
//
// It returns the offending reply separately from the error so a rate limit
// cannot be mistaken for the bot answering — the two call for opposite
// reactions, and conflating them turns an infrastructure hiccup into a false
// report of a security hole.
func (u *User) Silence(ctx context.Context, window time.Duration) (reply string, err error) {
	before, err := u.incoming(ctx)
	if err != nil {
		return "", err
	}
	baseline := len(before)

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(window):
	}

	after, err := u.incoming(ctx)
	if err != nil {
		return "", err
	}

	if len(after) > baseline {
		return lastText(after), nil
	}

	return "", nil
}

// WaitForSentMessageGone blocks until a message the user sent is no longer in
// the chat, used to check that the bot deleted a credential.
//
// The bot answers before it deletes, so checking the instant the reply lands
// is a coin flip; the deletion is what matters, not its exact timing.
func (u *User) WaitForSentMessageGone(ctx context.Context, text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		messages, err := u.history(ctx)
		if err != nil {
			return err
		}

		found := false
		for _, m := range messages {
			if m.Out && m.Message == text {
				found = true
				break
			}
		}
		if !found {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("message was still in the chat after %s", timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(replyPoll):
		}
	}
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
	res, err := u.getHistory(ctx)
	if err != nil {
		return nil, err
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

// getHistory fetches the chat, waiting out Telegram's flood control.
//
// Polling a chat is exactly the pattern that trips FLOOD_WAIT, and a test that
// fails on it reports a rate limit as though the bot misbehaved. Telegram says
// how long to wait, so wait.
func (u *User) getHistory(ctx context.Context) (tg.MessagesMessagesClass, error) {
	for attempt := 1; ; attempt++ {
		res, err := u.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  u.bot,
			Limit: historyLimit,
		})
		if err == nil {
			return res, nil
		}

		wait, isFlood := tgerr.AsFloodWait(err)
		if !isFlood || attempt > floodRetries {
			return nil, fmt.Errorf("get history: %w", err)
		}

		// Telegram's figure is a floor, so add a margin.
		wait += time.Second

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
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
