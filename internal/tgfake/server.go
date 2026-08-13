// Package tgfake is a fake Telegram Bot API server for tests.
//
// The bot under test points its API URL here and is otherwise untouched: a real
// process, a real HTTP client, a real long-polling loop, real command routing.
// Only Telegram is replaced. That covers the wiring where bots actually break —
// whether commands are reachable, routed, authorised, and answered — with no
// network, no credentials, and no rate limits, so it belongs in CI.
//
// It does not model Telegram's own behaviour: whether a delete is permitted,
// whether HTML parses, how flood control behaves. Those need the real thing.
package tgfake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pollTick is how often a waiting getUpdates re-checks the queue, and how often
// the assertion helpers re-check recorded calls.
const pollTick = 10 * time.Millisecond

// Call is one Bot API request the bot made.
type Call struct {
	Method string
	Params map[string]any
}

// Text returns a string parameter, empty when absent.
func (c Call) Text(key string) string {
	value, _ := c.Params[key].(string)
	return value
}

// Server is a fake Bot API endpoint.
type Server struct {
	http *httptest.Server

	mu       sync.Mutex
	updates  []map[string]any
	calls    []Call
	failures map[string]failure
	nextID   int64
}

type failure struct {
	status      int
	description string
}

// New starts a fake server. Close it when the test finishes.
func New() *Server {
	s := &Server{failures: map[string]failure{}, nextID: 1000}
	s.http = httptest.NewServer(http.HandlerFunc(s.handle))

	return s
}

// URL is what the bot under test should use as its Bot API base URL.
func (s *Server) URL() string {
	return s.http.URL
}

// Close shuts the server down.
func (s *Server) Close() {
	s.http.Close()
}

// SendCommand queues a message from chatID as if a user had typed it. Commands
// carry the entity Telegram attaches, without which a bot framework will not
// route them or populate the payload.
func (s *Server) SendCommand(chatID int64, text string) {
	command := text
	space := strings.IndexByte(text, ' ')
	if space > 0 {
		command = text[:space]
	}

	entities := []map[string]any{}
	if strings.HasPrefix(text, "/") {
		entities = append(entities, map[string]any{
			"type":   "bot_command",
			"offset": 0,
			"length": len(command),
		})
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	s.updates = append(s.updates, map[string]any{
		"update_id": s.nextID,
		"message": map[string]any{
			"message_id": s.nextID,
			"from": map[string]any{
				"id": chatID, "is_bot": false, "first_name": "Tester", "username": "tester",
			},
			"chat": map[string]any{"id": chatID, "type": "private"},
			"date": time.Now().Unix(),
			"text": text,
			// Entities are what make this a command rather than plain text.
			"entities": entities,
		},
	})
}

// FailNext makes the next call to method fail, for exercising the bot's error
// handling. Provoking a failure on demand is the main thing a fake can do that
// the real API cannot.
func (s *Server) FailNext(method string, status int, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures[method] = failure{status: status, description: description}
}

// Calls returns every request the bot has made, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Call(nil), s.calls...)
}

// WaitForCall blocks until the bot calls method with a parameter containing
// want ("" matches any), and returns it.
func (s *Server) WaitForCall(method, param, want string, timeout time.Duration) (Call, error) {
	deadline := time.Now().Add(timeout)

	for {
		for _, call := range s.Calls() {
			if call.Method != method {
				continue
			}
			if want == "" || strings.Contains(call.Text(param), want) {
				return call, nil
			}
		}

		if time.Now().After(deadline) {
			return Call{}, fmt.Errorf("no %s call with %s containing %q within %s; calls so far: %s",
				method, param, want, timeout, s.summary())
		}
		time.Sleep(pollTick)
	}
}

// WaitForMessage is the common case: a message sent to the chat.
func (s *Server) WaitForMessage(want string, timeout time.Duration) (string, error) {
	call, err := s.WaitForCall("sendMessage", "text", want, timeout)
	if err != nil {
		return "", err
	}

	return call.Text("text"), nil
}

// ExpectNoMessageTo reports the offending text if the bot messages chatID
// within the window, and an empty string when it stays silent.
//
// Scoping by chat is essential, not a refinement: a bot that is busy doing
// something else — reporting a startup failure to its own chat, say — will
// produce sendMessage calls that have nothing to do with what is being tested.
// An unscoped check reads those as "the bot answered a chat it should not have",
// which is a false report of a security hole.
//
// The text is returned rather than an error so a test can tell "the bot did
// something it should not have" apart from a harness problem.
func (s *Server) ExpectNoMessageTo(chatID int64, window time.Duration) string {
	deadline := time.Now().Add(window)

	for time.Now().Before(deadline) {
		for _, text := range s.MessagesTo(chatID) {
			return text
		}
		time.Sleep(pollTick)
	}

	return ""
}

// MessagesTo returns the text of every message the bot sent to chatID.
func (s *Server) MessagesTo(chatID int64) []string {
	want := strconv.FormatInt(chatID, 10)

	var texts []string
	for _, call := range s.Calls() {
		if call.Method != "sendMessage" {
			continue
		}
		if call.Text("chat_id") == want {
			texts = append(texts, call.Text("text"))
		}
	}

	return texts
}

func (s *Server) summary() string {
	var methods []string
	for _, call := range s.Calls() {
		if call.Method == "getUpdates" {
			continue
		}
		methods = append(methods, call.Method)
	}
	if len(methods) == 0 {
		return "(none besides getUpdates)"
	}

	return strings.Join(methods, ", ")
}
