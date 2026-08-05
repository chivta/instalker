package domain

import "time"

// Kind distinguishes the two feeds instalker watches.
type Kind string

const (
	KindPost  Kind = "post"
	KindStory Kind = "story"
)

// MediaType mirrors Instagram's numeric media_type field.
type MediaType int

const (
	MediaTypePhoto    MediaType = 1
	MediaTypeVideo    MediaType = 2
	MediaTypeCarousel MediaType = 8
)

// User is an Instagram account instalker knows about.
type User struct {
	PK        string
	Username  string
	FullName  string
	IsPrivate bool
}

// Attachment is a single downloadable file belonging to a Media.
type Attachment struct {
	URL     string
	IsVideo bool
}

// Media is one post or story item, normalised across both feeds.
type Media struct {
	ID          string
	Kind        Kind
	Type        MediaType
	Owner       User
	Caption     string
	TakenAt     time.Time
	Permalink   string
	Attachments []Attachment
}
