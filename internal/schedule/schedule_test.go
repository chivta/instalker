package schedule

import (
	"testing"
	"time"
)

func TestParseEmbeddedSchedule(t *testing.T) {
	plan, err := parse(embedded)
	if err != nil {
		t.Fatalf("the schedule shipped in the binary does not parse: %v", err)
	}

	if len(plan.Accounts) == 0 {
		t.Fatal("no accounts configured")
	}
	for _, account := range plan.Accounts {
		if account.Posts == 0 && account.Stories == 0 {
			t.Errorf("%s has nothing to poll", account.Username)
		}
	}
}

// The window runs from morning into the small hours, so the naive "from <= now
// < to" comparison is wrong for most of its length.
func TestActiveAcrossMidnight(t *testing.T) {
	plan, err := parse([]byte(`
timezone = "UTC"
[window]
from = "10:00"
to = "01:00"
[defaults]
posts = "1h"
stories = "1h"
[[accounts]]
username = "someone"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		hour, minute int
		want         bool
	}{
		{9, 59, false},
		{10, 0, true},
		{13, 30, true},
		{23, 59, true},
		{0, 30, true},
		{0, 59, true},
		{1, 0, false},
		{5, 0, false},
	}

	for _, tt := range tests {
		at := time.Date(2026, 8, 13, tt.hour, tt.minute, 0, 0, time.UTC)
		if got := plan.Active(at); got != tt.want {
			t.Errorf("Active(%02d:%02d) = %v, want %v", tt.hour, tt.minute, got, tt.want)
		}
	}
}

func TestPerAccountOverrides(t *testing.T) {
	plan, err := parse([]byte(`
timezone = "UTC"
[window]
from = "10:00"
to = "01:00"
[defaults]
posts = "1h"
stories = "1h"
[[accounts]]
username = "fast"
stories = "30m"
[[accounts]]
username = "plain"
[[accounts]]
username = "postsonly"
stories = "0"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fast, _ := plan.For("fast")
	if fast.Stories != 30*time.Minute || fast.Posts != time.Hour {
		t.Errorf("fast = %+v, want 30m stories and the default 1h posts", fast)
	}

	plain, _ := plan.For("plain")
	if plain.Posts != time.Hour || plain.Stories != time.Hour {
		t.Errorf("plain = %+v, want both defaults", plain)
	}

	// "0" switches a feed off rather than meaning "as often as possible".
	postsOnly, _ := plan.For("postsonly")
	if postsOnly.Stories != 0 {
		t.Errorf("postsonly stories = %s, want disabled", postsOnly.Stories)
	}

	if _, found := plan.For("nobody"); found {
		t.Error("an unlisted account was reported as configured")
	}
}

func TestRejectsBrokenSchedules(t *testing.T) {
	tests := map[string]string{
		"unknown timezone":  `timezone = "Mars/Olympus"` + "\n[window]\nfrom=\"10:00\"\nto=\"01:00\"\n",
		"window never open": "timezone=\"UTC\"\n[window]\nfrom=\"10:00\"\nto=\"10:00\"\n",
		"bad time":          "timezone=\"UTC\"\n[window]\nfrom=\"morning\"\nto=\"01:00\"\n",
		"bad duration":      "timezone=\"UTC\"\n[window]\nfrom=\"10:00\"\nto=\"01:00\"\n[defaults]\nposts=\"often\"\n",
		"account with everything off": "timezone=\"UTC\"\n[window]\nfrom=\"10:00\"\nto=\"01:00\"\n" +
			"[defaults]\nposts=\"1h\"\nstories=\"1h\"\n[[accounts]]\nusername=\"x\"\nposts=\"0\"\nstories=\"0\"\n",
		"account without a username": "timezone=\"UTC\"\n[window]\nfrom=\"10:00\"\nto=\"01:00\"\n[[accounts]]\nusername=\"\"\n",
	}

	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parse([]byte(document))
			if err == nil {
				t.Fatal("expected an error")
			}
			t.Logf("%v", err)
		})
	}
}
