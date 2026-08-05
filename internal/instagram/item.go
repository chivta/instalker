package instagram

import (
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// item is the shape Instagram uses for a single media entry, shared by the
// timeline feed and the story reel feed.
type item struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	TakenAt   int64  `json:"taken_at"`
	MediaType int    `json:"media_type"`
	Caption   *struct {
		Text string `json:"text"`
	} `json:"caption"`
	ImageVersions2 struct {
		Candidates []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"candidates"`
	} `json:"image_versions2"`
	VideoVersions []struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"video_versions"`
	CarouselMedia []item `json:"carousel_media"`
}

func (i item) toMedia(kind domain.Kind, owner domain.User) domain.Media {
	media := domain.Media{
		ID:          i.ID,
		Kind:        kind,
		Type:        domain.MediaType(i.MediaType),
		Owner:       owner,
		TakenAt:     time.Unix(i.TakenAt, 0).UTC(),
		Attachments: i.attachments(),
	}

	if i.Caption != nil {
		media.Caption = i.Caption.Text
	}
	if kind == domain.KindPost && i.Code != "" {
		media.Permalink = baseURL + "/p/" + i.Code + "/"
	}

	return media
}

// attachments flattens a media entry into the files worth sending, picking the
// highest resolution candidate available for each.
func (i item) attachments() []domain.Attachment {
	if len(i.CarouselMedia) > 0 {
		var out []domain.Attachment
		for _, child := range i.CarouselMedia {
			out = append(out, child.attachments()...)
		}
		return out
	}

	if len(i.VideoVersions) > 0 {
		best := i.VideoVersions[0]
		for _, v := range i.VideoVersions {
			if v.Width*v.Height > best.Width*best.Height {
				best = v
			}
		}
		return []domain.Attachment{{URL: best.URL, IsVideo: true}}
	}

	candidates := i.ImageVersions2.Candidates
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, cand := range candidates {
		if cand.Width*cand.Height > best.Width*best.Height {
			best = cand
		}
	}

	return []domain.Attachment{{URL: best.URL, IsVideo: false}}
}
