package tgtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// botAPITimeout bounds the single Bot API call made here.
const botAPITimeout = 15 * time.Second

// BotUsername asks Telegram who a bot token belongs to.
//
// Deriving it removes a thing to configure and, more usefully, fails loudly and
// early when the staging token is wrong — otherwise the tests would resolve
// some other bot and quietly time out waiting for replies.
func BotUsername(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, botAPITimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/bot"+token+"/getMe", nil)
	if err != nil {
		return "", fmt.Errorf("build getMe request: %w", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call getMe: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read getMe: %w", err)
	}

	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
		} `json:"result"`
	}

	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return "", fmt.Errorf("decode getMe: %w", err)
	}
	if !parsed.OK {
		return "", fmt.Errorf("getMe rejected the token: %s", parsed.Description)
	}

	return parsed.Result.Username, nil
}
