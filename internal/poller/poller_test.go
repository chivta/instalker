package poller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

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
			p := New(&fakeInsta{posts: tt.posts, stories: tt.stories}, repo, sender, []domain.User{owner}, time.Minute)

			// A cancelled context still runs the cycle body; it only skips the
			// inter-send delay, which keeps the test fast.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := p.pollTarget(ctx, owner)
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
	p := New(insta, repo, sender, []domain.User{owner}, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.pollTarget(ctx, owner)
	if err != nil {
		t.Fatalf("baseline cycle: %v", err)
	}

	insta.posts = append([]domain.Media{media("p2", domain.KindPost, owner)}, insta.posts...)

	err = p.pollTarget(ctx, owner)
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
	p := New(insta, repo, sender, []domain.User{owner}, time.Minute)

	// cycle short-circuits on a cancelled context, so this one must stay live.
	// No media is delivered in either phase, so there is no send delay to wait on.
	ctx := context.Background()

	// Two broken cycles must produce exactly one alert, not one per tick.
	p.cycle(ctx)
	p.cycle(ctx)

	if len(sender.notices) != 1 {
		t.Fatalf("got %d notices, want 1: %v", len(sender.notices), sender.notices)
	}
	if !strings.Contains(sender.notices[0], "refusing the session") {
		t.Errorf("alert did not mention the session: %q", sender.notices[0])
	}

	// Recovery is announced once.
	insta.postsErr, insta.storiesErr = nil, nil
	p.cycle(ctx)
	p.cycle(ctx)

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
	p := New(insta, repo, sender, []domain.User{owner}, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = p.pollTarget(ctx, owner)

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
	p := New(insta, repo, sender, []domain.User{owner}, time.Minute)

	p.cycle(context.Background())

	if len(sender.notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(sender.notices))
	}
	if !strings.Contains(sender.notices[0], "rate limiting") {
		t.Errorf("alert should name throttling, got %q", sender.notices[0])
	}
	// Rotating the cookie does not clear a 429, so the alert must not ask for one.
	if strings.Contains(sender.notices[0], "IG_SESSIONID") {
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
