// Command login performs the one-time interactive Telegram login for the spare
// account used by the end-to-end tests.
//
// Telegram sends the login code to the account itself, so this step cannot be
// automated. Run it once; it writes a session file that the tests reuse until
// it is revoked.
//
//	go run ./test/e2e/login
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"github.com/arvlas/instalker/test/e2e/tgtest"
)

func main() {
	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := tgtest.LoadConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: cfg.SessionStorage(),
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}

		if !status.Authorized {
			flow := auth.NewFlow(terminalAuth{phone: cfg.Phone}, auth.SendCodeOptions{})
			err = flow.Run(ctx, client.Auth())
			if err != nil {
				return fmt.Errorf("auth flow: %w", err)
			}
		}

		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}

		fmt.Printf("\nlogged in as @%s (id %d)\n", self.Username, self.ID)
		fmt.Printf("session written to %s\n", cfg.SessionFile)
		fmt.Printf("\nadd this to %s:\n  TG_USER_ID=%d\n", tgtest.ConfigFile, self.ID)

		return nil
	})
}

// terminalAuth answers Telegram's auth questions from stdin.
type terminalAuth struct {
	phone string
}

func (t terminalAuth) Phone(context.Context) (string, error) {
	if t.phone != "" {
		return t.phone, nil
	}
	return prompt("phone number (with country code, e.g. +123456789): ")
}

func (terminalAuth) Password(context.Context) (string, error) {
	return prompt("2FA password (blank if none): ")
}

func (terminalAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	return prompt("login code Telegram just sent: ")
}

func (terminalAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Println("terms of service:", tos.Text)
	return nil
}

func (terminalAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("the spare account must already exist; sign-up is not supported here")
}

func prompt(label string) (string, error) {
	fmt.Print(label)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read %s: %w", strings.TrimSpace(label), err)
	}

	return strings.TrimSpace(line), nil
}
