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
	"time"

	"golang.org/x/term"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"github.com/arvlas/instalker/test/e2e/tgtest"
)

const (
	// codeFile and passwordFile are where answers are read from when no
	// terminal is attached.
	codeFile     = ".e2e-code"
	passwordFile = ".e2e-2fa"

	answerWait = 5 * time.Minute
	answerPoll = time.Second
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
		fmt.Printf("\nthe chat id is derived from this account at run time; nothing else to configure\n")

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
	return prompt("phone number (with country code, e.g. +123456789): ", codeFile)
}

func (terminalAuth) Password(context.Context) (string, error) {
	// Set TG_2FA_PASSWORD to skip this entirely; unlike the code it does not
	// expire, so it does not need to be answered live.
	password, ok := os.LookupEnv("TG_2FA_PASSWORD")
	if ok {
		return password, nil
	}

	return prompt("2FA password (blank if none): ", passwordFile)
}

func (terminalAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	return prompt("login code Telegram just sent: ", codeFile)
}

func (terminalAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Println("terms of service:", tos.Text)
	return nil
}

func (terminalAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("the spare account must already exist; sign-up is not supported here")
}

// prompt reads an answer from the terminal, or — when there is no terminal, as
// when this runs from a tool rather than a shell — from a file the operator
// writes instead. Telegram will not send the code twice, so failing on a
// missing stdin would waste it.
func prompt(label, file string) (string, error) {
	if isTerminal(os.Stdin) {
		fmt.Print(label)

		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read %s: %w", strings.TrimSpace(label), err)
		}

		return strings.TrimSpace(line), nil
	}

	fmt.Printf("\nno terminal attached. Write the answer to %s, e.g.\n\n    echo '123456' > %s\n\nwaiting up to %s...\n", file, file, answerWait)

	return waitForFile(file)
}

// isTerminal must not settle for "is a character device": when this runs as a
// background command stdin is /dev/null, which passes that test and then reads
// EOF — burning a login code Telegram will not resend indefinitely.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// waitForFile polls for the operator's answer, then removes it: the login code
// is single-use and the 2FA password is a credential.
func waitForFile(path string) (string, error) {
	deadline := time.Now().Add(answerWait)

	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			answer := strings.TrimSpace(string(raw))
			if answer != "" {
				_ = os.Remove(path)
				fmt.Printf("got it (%d chars)\n", len(answer))
				return answer, nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("nothing written to %s within %s", path, answerWait)
		}
		time.Sleep(answerPoll)
	}
}
