package poller

import (
	"context"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

type fakeInsta struct {
	posts   []domain.Media
	stories []domain.Media
}

func (f *fakeInsta) Profile(context.Context, string) (domain.User, error) {
	return domain.User{}, nil
}

func (f *fakeInsta) Following(context.Context, string) ([]domain.User, error) {
	return nil, nil
}

func (f *fakeInsta) Posts(context.Context, domain.User) ([]domain.Media, error) {
	return f.posts, nil
}

func (f *fakeInsta) Stories(context.Context, domain.User) ([]domain.Media, error) {
	return f.stories, nil
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
	sent []domain.Media
}

func (f *fakeSender) Send(_ context.Context, m domain.Media) error {
	f.sent = append(f.sent, m)
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
