package poller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/schedule"
)

// bothFeeds is what a caller passes when it wants a full poll of a target.
var bothFeeds = []domain.Kind{domain.KindPost, domain.KindStory}

type fakeInsta struct {
	posts        []domain.Media
	stories      []domain.Media
	postsErr     error
	storiesErr   error
	followingErr error
}

func (f *fakeInsta) Profile(context.Context, string) (domain.User, error) {
	return domain.User{}, nil
}

func (f *fakeInsta) Following(context.Context, string) ([]domain.User, error) {
	return nil, f.followingErr
}

func (f *fakeInsta) Posts(context.Context, domain.User) ([]domain.Media, error) {
	return f.posts, f.postsErr
}

func (f *fakeInsta) Stories(context.Context, domain.User) ([]domain.Media, error) {
	return f.stories, f.storiesErr
}

type fakeRepo struct {
	seen        map[string]bool
	initialized bool
}

func (f *fakeRepo) Seen(_ context.Context, ownerPK string, kind domain.Kind, mediaID string) (bool, error) {
	return f.seen[ownerPK+string(kind)+mediaID], nil
}

func (f *fakeRepo) MarkSeen(_ context.Context, m domain.Media) error {
	f.seen[m.Owner.PK+string(m.Kind)+m.ID] = true
	return nil
}

func (f *fakeRepo) Initialized(context.Context, string) (bool, error) {
	return f.initialized, nil
}

func (f *fakeRepo) MarkInitialized(context.Context, domain.User) error {
	f.initialized = true
	return nil
}

type fakeSender struct {
	sent    []domain.Media
	notices []string
}

func (f *fakeSender) Send(_ context.Context, m domain.Media) error {
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) Notify(_ context.Context, text string) error {
	f.notices = append(f.notices, text)
	return nil
}

func media(id string, kind domain.Kind, owner domain.User) domain.Media {
	return domain.Media{ID: id, Kind: kind, Owner: owner, TakenAt: time.Unix(1700000000, 0)}
}

func TestPollTarget(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}

	tests := []struct {
		name        string
		initialized bool
		seen        map[string]bool
		posts       []domain.Media
		stories     []domain.Media
		wantSent    []string
	}{
		{
			name:        "first cycle only baselines",
			initialized: false,
			posts:       []domain.Media{media("p1", domain.KindPost, owner)},
			stories:     []domain.Media{media("s1", domain.KindStory, owner)},
			wantSent:    nil,
		},
		{
			name:        "new media is delivered oldest first",
			initialized: true,
			posts:       []domain.Media{media("p2", domain.KindPost, owner), media("p1", domain.KindPost, owner)},
			wantSent:    []string{"p1", "p2"},
		},
		{
			name:        "already seen media is skipped",
			initialized: true,
			seen:        map[string]bool{"1postp1": true},
			posts:       []domain.Media{media("p2", domain.KindPost, owner), media("p1", domain.KindPost, owner)},
			wantSent:    []string{"p2"},
		},
		{
			name:        "posts and stories are both delivered",
			initialized: true,
			posts:       []domain.Media{media("p1", domain.KindPost, owner)},
			stories:     []domain.Media{media("s1", domain.KindStory, owner)},
			wantSent:    []string{"p1", "s1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := tt.seen
			if seen == nil {
				seen = map[string]bool{}
			}

			repo := &fakeRepo{seen: seen, initialized: tt.initialized}
			sender := &fakeSender{}
			p := New(&fakeInsta{posts: tt.posts, stories: tt.stories}, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

			// A cancelled context still runs the cycle body; it only skips the
			// inter-send delay, which keeps the test fast.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := p.pollTarget(ctx, owner, bothFeeds, time.Now())
			if err != nil {
				t.Fatalf("pollTarget: %v", err)
			}

			if len(sender.sent) != len(tt.wantSent) {
				t.Fatalf("sent %d media, want %d (%v)", len(sender.sent), len(tt.wantSent), ids(sender.sent))
			}
			for i, want := range tt.wantSent {
				if sender.sent[i].ID != want {
					t.Errorf("sent[%d] = %s, want %s", i, sender.sent[i].ID, want)
				}
			}
		})
	}
}

func TestPollTargetMarksSeenAfterBaseline(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}}
	sender := &fakeSender{}
	insta := &fakeInsta{posts: []domain.Media{media("p1", domain.KindPost, owner)}}
	p := New(insta, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.pollTarget(ctx, owner, bothFeeds, time.Now())
	if err != nil {
		t.Fatalf("baseline cycle: %v", err)
	}

	insta.posts = append([]domain.Media{media("p2", domain.KindPost, owner)}, insta.posts...)

	err = p.pollTarget(ctx, owner, bothFeeds, time.Now())
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}

	if len(sender.sent) != 1 || sender.sent[0].ID != "p2" {
		t.Fatalf("sent %v, want only p2", ids(sender.sent))
	}
}

func ids(media []domain.Media) []string {
	out := make([]string, 0, len(media))
	for _, m := range media {
		out = append(out, m.ID)
	}

	return out
}

func TestAuthFailureAlertsOnceAndRecovers(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	sender := &fakeSender{}
	insta := &fakeInsta{
		postsErr:   fmt.Errorf("posts target: %w", domain.ErrUnauthorized),
		storiesErr: fmt.Errorf("stories target: %w", domain.ErrUnauthorized),
	}
	p := New(insta, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	// cycle short-circuits on a cancelled context, so this one must stay live.
	// No media is delivered in either phase, so there is no send delay to wait on.
	ctx := context.Background()

	// Two broken cycles must produce exactly one alert, not one per tick.
	p.tick(ctx, time.Now())
	p.tick(ctx, time.Now())

	if len(sender.notices) != 1 {
		t.Fatalf("got %d notices, want 1: %v", len(sender.notices), sender.notices)
	}
	if !strings.Contains(sender.notices[0], "refusing the session") {
		t.Errorf("alert did not mention the session: %q", sender.notices[0])
	}

	// Recovery is announced once.
	insta.postsErr, insta.storiesErr = nil, nil
	p.tick(ctx, time.Now())
	p.tick(ctx, time.Now())

	if len(sender.notices) != 2 {
		t.Fatalf("got %d notices after recovery, want 2: %v", len(sender.notices), sender.notices)
	}
	if !strings.Contains(sender.notices[1], "resumed") {
		t.Errorf("recovery notice unexpected: %q", sender.notices[1])
	}
}

func TestPartialFetchDoesNotBaseline(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}}
	sender := &fakeSender{}
	insta := &fakeInsta{
		posts:      []domain.Media{media("p1", domain.KindPost, owner)},
		storiesErr: fmt.Errorf("stories target: %w", domain.ErrUnauthorized),
	}
	p := New(insta, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = p.pollTarget(ctx, owner, bothFeeds, time.Now())

	if repo.initialized {
		t.Fatal("target was baselined despite a failed feed")
	}
}

func TestRateLimitAlertDoesNotAskForNewSession(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	sender := &fakeSender{}
	insta := &fakeInsta{
		postsErr:   fmt.Errorf("posts target: %w", domain.ErrRateLimited),
		storiesErr: fmt.Errorf("stories target: %w", domain.ErrRateLimited),
	}
	p := New(insta, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	p.tick(context.Background(), time.Now())

	if len(sender.notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(sender.notices))
	}
	if !strings.Contains(sender.notices[0], "rate limiting") {
		t.Errorf("alert should name throttling, got %q", sender.notices[0])
	}
	// Rotating the cookie does not clear a 429, so the alert must not ask for one.
	if strings.Contains(sender.notices[0], "/session") {
		t.Errorf("rate limit alert wrongly asks for a new session: %q", sender.notices[0])
	}
}

// ResolveTargets must not hide why it failed: the caller decides whether to
// retry based on the cause, and %v wrapping silently made everything fatal.
func TestResolveTargetsPreservesCause(t *testing.T) {
	failing := &fakeInsta{followingErr: fmt.Errorf("following 1: %w", domain.ErrRateLimited)}

	_, err := ResolveTargets(context.Background(), failing, domain.User{PK: "1", Username: "me"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, domain.ErrTargetsUnresolved) {
		t.Errorf("cause lost ErrTargetsUnresolved: %v", err)
	}
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("cause lost ErrRateLimited, retry logic would treat it as fatal: %v", err)
	}
}

func TestProbe(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	older := domain.Media{ID: "p1", Kind: domain.KindPost, Owner: owner, TakenAt: time.Unix(1700000000, 0)}
	newer := domain.Media{ID: "s1", Kind: domain.KindStory, Owner: owner, TakenAt: time.Unix(1700009999, 0)}

	t.Run("reports counts and newest timestamp across both feeds", func(t *testing.T) {
		insta := &fakeInsta{posts: []domain.Media{older}, stories: []domain.Media{newer}}
		repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
		p := New(insta, repo, &fakeSender{}, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

		got := p.Probe(context.Background())

		if !got.OK() {
			t.Fatalf("probe not ok: %v", got.Targets[0].Err)
		}
		if got.Targets[0].Posts != 1 || got.Targets[0].Stories != 1 {
			t.Errorf("counts = %d posts / %d stories, want 1/1", got.Targets[0].Posts, got.Targets[0].Stories)
		}
		if !got.Targets[0].Latest.Equal(newer.TakenAt) {
			t.Errorf("latest = %v, want the story timestamp %v", got.Targets[0].Latest, newer.TakenAt)
		}
	})

	t.Run("surfaces a feed failure", func(t *testing.T) {
		insta := &fakeInsta{postsErr: fmt.Errorf("posts: %w", domain.ErrRateLimited)}
		repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
		p := New(insta, repo, &fakeSender{}, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

		got := p.Probe(context.Background())

		if got.OK() {
			t.Fatal("probe reported ok despite a failing feed")
		}
		if !errors.Is(got.Targets[0].Err, domain.ErrRateLimited) {
			t.Errorf("err = %v, want rate limited", got.Targets[0].Err)
		}
	})

	t.Run("does not deliver or mark anything seen", func(t *testing.T) {
		insta := &fakeInsta{posts: []domain.Media{older}, stories: []domain.Media{newer}}
		repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
		sender := &fakeSender{}
		p := New(insta, repo, sender, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

		p.Probe(context.Background())

		if len(sender.sent) != 0 {
			t.Errorf("probe delivered %d media, want 0", len(sender.sent))
		}
		if len(repo.seen) != 0 {
			t.Errorf("probe marked %d media seen, want 0", len(repo.seen))
		}
	})
}

// Polling on schedule through a throttle is what keeps the throttle alive, so a
// rate-limited cycle must report itself and a clean one must clear.
func TestCycleReportsThrottling(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	insta := &fakeInsta{
		postsErr:   fmt.Errorf("posts: %w", domain.ErrRateLimited),
		storiesErr: fmt.Errorf("stories: %w", domain.ErrRateLimited),
	}
	p := New(insta, repo, &fakeSender{}, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	now := time.Now()
	p.tick(context.Background(), now)
	if !p.backoffUntil.After(now) {
		t.Fatal("a rate-limited tick did not pause polling")
	}

	// Clear the pause so the next tick runs, then check it is not re-armed.
	insta.postsErr, insta.storiesErr = nil, nil
	p.backoffUntil = time.Time{}

	now = time.Now()
	p.tick(context.Background(), now)
	if p.backoffUntil.After(now) {
		t.Fatal("a clean tick still paused polling")
	}
}

// An auth failure is not a throttle: backing off would delay the recovery the
// user is being asked to perform.
func TestAuthFailureIsNotThrottling(t *testing.T) {
	owner := domain.User{PK: "1", Username: "target"}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	insta := &fakeInsta{
		postsErr:   fmt.Errorf("posts: %w", domain.ErrUnauthorized),
		storiesErr: fmt.Errorf("stories: %w", domain.ErrUnauthorized),
	}
	p := New(insta, repo, &fakeSender{}, []domain.User{owner}, schedule.Always(time.Minute, time.Minute))

	now := time.Now()
	p.tick(context.Background(), now)

	if p.backoffUntil.After(now) {
		t.Fatal("an auth failure paused polling, delaying the /session recovery it asks for")
	}
}

func TestJitterStaysNearTheBaseDelay(t *testing.T) {
	const base = 8 * time.Second

	for range 200 {
		got := jitter(base)
		if got < base*3/4 || got > base*5/4 {
			t.Fatalf("jitter(%s) = %s, want within ±25%%", base, got)
		}
	}
}

// The schedule's whole point is that feeds run at different cadences, so a due
// check that ignores either the interval or the window would go unnoticed.
func TestDueRespectsIntervalsAndWindow(t *testing.T) {
	plan, err := schedule.Load()
	if err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	locroise := domain.User{PK: "1", Username: "locroise"}
	lem1rol := domain.User{PK: "2", Username: "lem1rol"}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	p := New(&fakeInsta{}, repo, &fakeSender{}, []domain.User{locroise, lem1rol}, plan)

	ctx := context.Background()
	start := time.Date(2026, 8, 13, 12, 0, 0, 0, plan.Location)

	// Nothing has run yet, so both feeds are due for both accounts.
	if got := p.due(ctx, locroise, start); len(got) != 2 {
		t.Fatalf("first poll due = %v, want both feeds", got)
	}

	p.markFetched(locroise.PK, domain.KindPost, start)
	p.markFetched(locroise.PK, domain.KindStory, start)
	p.markFetched(lem1rol.PK, domain.KindPost, start)
	p.markFetched(lem1rol.PK, domain.KindStory, start)

	// Half an hour on: only locroise's stories are configured that often.
	half := start.Add(30 * time.Minute)
	if got := p.due(ctx, locroise, half); len(got) != 1 || got[0] != domain.KindStory {
		t.Errorf("locroise at +30m = %v, want stories only", got)
	}
	if got := p.due(ctx, lem1rol, half); len(got) != 0 {
		t.Errorf("lem1rol at +30m = %v, want nothing", got)
	}

	// An hour on, everything is due again.
	hour := start.Add(time.Hour)
	if got := p.due(ctx, lem1rol, hour); len(got) != 2 {
		t.Errorf("lem1rol at +1h = %v, want both feeds", got)
	}
}

// Outside the window nothing is fetched at all, however overdue it looks.
func TestTickSkipsOutsideTheWindow(t *testing.T) {
	plan, err := schedule.Load()
	if err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	owner := domain.User{PK: "1", Username: "locroise"}
	insta := &fakeInsta{posts: []domain.Media{media("p1", domain.KindPost, owner)}}
	repo := &fakeRepo{seen: map[string]bool{}, initialized: true}
	sender := &fakeSender{}
	p := New(insta, repo, sender, []domain.User{owner}, plan)

	// 04:00 is outside the configured 10:00–01:00 window.
	p.tick(context.Background(), time.Date(2026, 8, 13, 4, 0, 0, 0, plan.Location))

	if len(sender.sent) != 0 {
		t.Errorf("delivered %d media outside the window, want 0", len(sender.sent))
	}
	if len(p.lastPosts) != 0 {
		t.Error("a feed was fetched outside the window")
	}
}
