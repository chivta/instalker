package tgfake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// longPollWait bounds how long getUpdates holds a request open. Real Telegram
// blocks for the caller's timeout; blocking here too keeps the bot's polling
// loop from spinning, and returning well before the caller's own deadline keeps
// shutdown responsive.
const longPollWait = 2 * time.Second

// handle routes /bot<token>/<method>.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "bot") {
		http.NotFound(w, r)
		return
	}
	method := parts[1]

	params := readParams(r)
	if method != "getUpdates" {
		s.record(Call{Method: method, Params: params})
	}

	fail, failing := s.takeFailure(method)
	if failing {
		writeJSON(w, fail.status, map[string]any{
			"ok": false, "error_code": fail.status, "description": fail.description,
		})
		return
	}

	switch method {
	case "getMe":
		writeJSON(w, http.StatusOK, ok(map[string]any{
			"id": 424242, "is_bot": true, "first_name": "Fake", "username": "fake_test_bot",
		}))
	case "getUpdates":
		writeJSON(w, http.StatusOK, ok(s.takeUpdates(params)))
	case "sendMessage", "sendPhoto", "sendVideo":
		writeJSON(w, http.StatusOK, ok(s.newMessage(params)))
	case "sendMediaGroup":
		writeJSON(w, http.StatusOK, ok([]any{s.newMessage(params)}))
	case "deleteMessage", "setMyCommands":
		writeJSON(w, http.StatusOK, ok(true))
	default:
		// Unknown methods succeed rather than error: the point is the bot's
		// behaviour, not completeness of the API surface. Every call is still
		// recorded, so a test can assert on one this server does not model.
		writeJSON(w, http.StatusOK, ok(true))
	}
}

// takeUpdates returns queued updates, honouring the offset the bot acknowledges
// with, and waits a little when there is nothing rather than busy-looping.
func (s *Server) takeUpdates(params map[string]any) []map[string]any {
	deadline := time.Now().Add(longPollWait)

	for {
		s.mu.Lock()
		if len(s.updates) > 0 {
			updates := s.updates
			s.updates = nil
			s.mu.Unlock()
			return updates
		}
		s.mu.Unlock()

		if time.Now().After(deadline) {
			return []map[string]any{}
		}
		time.Sleep(pollTick)
	}
}

// newMessage is a plausible sent-message result, which the bot's framework
// decodes and would reject if it were empty.
func (s *Server) newMessage(params map[string]any) map[string]any {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()

	chatID := int64(0)
	if raw, ok := params["chat_id"].(string); ok {
		chatID, _ = strconv.ParseInt(raw, 10, 64)
	}

	text, _ := params["text"].(string)

	return map[string]any{
		"message_id": id,
		"from":       map[string]any{"id": 424242, "is_bot": true, "first_name": "Fake"},
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"date":       time.Now().Unix(),
		"text":       text,
	}
}

func (s *Server) record(call Call) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, call)
}

func (s *Server) takeFailure(method string) (failure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fail, ok := s.failures[method]
	if ok {
		delete(s.failures, method)
	}

	return fail, ok
}

// readParams accepts both encodings a bot framework may use: form values and a
// JSON body.
func readParams(r *http.Request) map[string]any {
	params := map[string]any{}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	defer r.Body.Close()

	contentType := r.Header.Get("content-type")
	switch {
	case strings.Contains(contentType, "json"):
		_ = json.Unmarshal(body, &params)
	default:
		values, err := parseForm(string(body))
		if err == nil {
			for key, value := range values {
				params[key] = value
			}
		}
	}

	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	return params
}

func parseForm(body string) (map[string]string, error) {
	values, err := url.ParseQuery(body)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for key, list := range values {
		if len(list) > 0 {
			out[key] = list[0]
		}
	}

	return out, nil
}

func ok(result any) map[string]any {
	return map[string]any{"ok": true, "result": result}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
