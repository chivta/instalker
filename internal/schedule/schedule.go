// Package schedule decides when each account is checked.
//
// The schedule is compiled into the binary rather than passed as environment
// variables: it is structured data — a window, per-account intervals per feed —
// which flattens badly into env vars, and it changes for editorial reasons
// ("check stories more often") rather than deployment ones.
package schedule

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// PathEnv names a file to load instead of the embedded schedule.
const PathEnv = "SCHEDULE_PATH"

//go:embed schedule.toml
var embedded []byte

// timeLayout is how the window bounds are written.
const timeLayout = "15:04"

// file mirrors the TOML document.
type file struct {
	Timezone string `toml:"timezone"`
	Window   struct {
		From string `toml:"from"`
		To   string `toml:"to"`
	} `toml:"window"`
	Defaults feeds     `toml:"defaults"`
	Accounts []account `toml:"accounts"`
}

type feeds struct {
	Posts   string `toml:"posts"`
	Stories string `toml:"stories"`
}

type account struct {
	Username string `toml:"username"`
	Posts    string `toml:"posts"`
	Stories  string `toml:"stories"`
}

// Plan is a validated schedule.
type Plan struct {
	Location *time.Location
	// From and To are offsets from midnight. To may be less than From, which
	// means the window crosses midnight.
	From, To time.Duration

	// defaults apply to accounts the schedule does not name, which happens when
	// targets come from TARGETS or the following list instead.
	defaults Account

	Accounts []Account
}

// Defaults returns the intervals used for accounts the schedule does not name.
func (p *Plan) Defaults() Account {
	return p.defaults
}

// Account is one watched account and how often each of its feeds is checked.
// A zero interval disables that feed.
type Account struct {
	Username string
	Posts    time.Duration
	Stories  time.Duration
}

// Always builds a plan with no closed hours and one interval for every feed.
//
// It exists for callers that want the poller without scheduling — tests, mostly
// — so they do not have to depend on the wall clock falling inside a window.
func Always(posts, stories time.Duration) *Plan {
	return &Plan{
		Location: time.UTC,
		From:     0,
		To:       24 * time.Hour,
		defaults: Account{Posts: posts, Stories: stories},
	}
}

// Load reads the schedule, preferring the file named by SCHEDULE_PATH and
// falling back to the copy compiled into the binary.
func Load() (*Plan, error) {
	raw := embedded
	source := "embedded schedule.toml"

	path, ok := os.LookupEnv(PathEnv)
	if ok && path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		raw = contents
		source = path
	}

	plan, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}

	return plan, nil
}

func parse(raw []byte) (*Plan, error) {
	var parsed file

	err := toml.Unmarshal(raw, &parsed)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	// A missing zone database is the classic way this breaks in a container, so
	// say which zone failed rather than passing the bare error up.
	location, err := time.LoadLocation(parsed.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", parsed.Timezone, err)
	}

	from, err := parseClock(parsed.Window.From)
	if err != nil {
		return nil, fmt.Errorf("window.from: %w", err)
	}

	to, err := parseClock(parsed.Window.To)
	if err != nil {
		return nil, fmt.Errorf("window.to: %w", err)
	}
	if from == to {
		return nil, fmt.Errorf("window.from and window.to are both %s, which is never open", parsed.Window.From)
	}

	defaultPosts, err := parseInterval(parsed.Defaults.Posts)
	if err != nil {
		return nil, fmt.Errorf("defaults.posts: %w", err)
	}

	defaultStories, err := parseInterval(parsed.Defaults.Stories)
	if err != nil {
		return nil, fmt.Errorf("defaults.stories: %w", err)
	}

	plan := &Plan{
		Location: location,
		From:     from,
		To:       to,
		defaults: Account{Posts: defaultPosts, Stories: defaultStories},
	}

	for _, entry := range parsed.Accounts {
		username := strings.TrimPrefix(strings.TrimSpace(entry.Username), "@")
		if username == "" {
			return nil, fmt.Errorf("an account entry has no username")
		}

		posts, err := interval(entry.Posts, defaultPosts)
		if err != nil {
			return nil, fmt.Errorf("%s.posts: %w", username, err)
		}

		stories, err := interval(entry.Stories, defaultStories)
		if err != nil {
			return nil, fmt.Errorf("%s.stories: %w", username, err)
		}

		if posts == 0 && stories == 0 {
			return nil, fmt.Errorf("%s has both feeds disabled", username)
		}

		plan.Accounts = append(plan.Accounts, Account{
			Username: username,
			Posts:    posts,
			Stories:  stories,
		})
	}

	return plan, nil
}

// Active reports whether t falls inside the polling window.
func (p *Plan) Active(t time.Time) bool {
	local := t.In(p.Location)
	since := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute

	if p.From < p.To {
		return since >= p.From && since < p.To
	}

	// The window crosses midnight, so it is open at both ends of the day.
	return since >= p.From || since < p.To
}

// For returns the intervals configured for a username, and whether it is listed
// at all.
func (p *Plan) For(username string) (Account, bool) {
	for _, entry := range p.Accounts {
		if strings.EqualFold(entry.Username, username) {
			return entry, true
		}
	}

	return Account{}, false
}

// Usernames lists the accounts the schedule names, in order.
func (p *Plan) Usernames() []string {
	out := make([]string, 0, len(p.Accounts))
	for _, entry := range p.Accounts {
		out = append(out, entry.Username)
	}

	return out
}

// Window renders the polling window for logs and chat messages.
func (p *Plan) Window() string {
	return fmt.Sprintf("%s–%s %s", clock(p.From), clock(p.To), p.Location)
}

func parseClock(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("missing, expected a time like 09:30")
	}

	parsed, err := time.Parse(timeLayout, strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time like 09:30", value)
	}

	return time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute, nil
}

func clock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

// interval resolves an account's override against the default.
func interval(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	return parseInterval(value)
}

func parseInterval(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "0" {
		return 0, nil
	}

	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 30m", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%q is negative", value)
	}

	return parsed, nil
}
