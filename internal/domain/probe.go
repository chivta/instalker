package domain

import "time"

// FeedProbe is the outcome of checking one feed of one account.
type FeedProbe struct {
	Count int
	// Latest is the timestamp of the most recent item, zero when none.
	Latest time.Time
	// Err is set when the feed could not be read.
	Err error
}

// OK reports whether the feed could be read.
func (f FeedProbe) OK() bool {
	return f.Err == nil
}

// TargetProbe is the outcome of a live scrape check for one watched account.
//
// The feeds are reported separately because Instagram throttles per endpoint:
// it will block the timeline while serving stories normally, and collapsing
// that into one verdict hides a working half of the bot.
type TargetProbe struct {
	User    User
	Posts   FeedProbe
	Stories FeedProbe
}

// OK reports whether both feeds could be read.
func (t TargetProbe) OK() bool {
	return t.Posts.OK() && t.Stories.OK()
}

// Failed reports whether neither feed could be read.
func (t TargetProbe) Failed() bool {
	return !t.Posts.OK() && !t.Stories.OK()
}

// Latest is the most recent item across both feeds, zero when there is none.
func (t TargetProbe) Latest() time.Time {
	latest := t.Posts.Latest
	if t.Stories.Latest.After(latest) {
		latest = t.Stories.Latest
	}

	return latest
}

// Probe is the result of checking every watched account.
type Probe struct {
	Targets []TargetProbe
	Elapsed time.Duration
}

// OK reports whether every feed of every target could be read.
func (p Probe) OK() bool {
	for _, t := range p.Targets {
		if !t.OK() {
			return false
		}
	}

	return len(p.Targets) > 0
}

// Failed reports whether nothing could be read at all.
func (p Probe) Failed() bool {
	for _, t := range p.Targets {
		if !t.Failed() {
			return false
		}
	}

	return len(p.Targets) > 0
}
