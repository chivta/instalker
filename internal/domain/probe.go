package domain

import "time"

// TargetProbe is the outcome of a live scrape check for one watched account.
type TargetProbe struct {
	User    User
	Posts   int
	Stories int
	// Latest is the timestamp of the most recent item seen, zero when none.
	Latest time.Time
	// Err is set when either feed could not be read.
	Err error
}

// OK reports whether the target could be scraped.
func (t TargetProbe) OK() bool {
	return t.Err == nil
}

// Probe is the result of checking every watched account.
type Probe struct {
	Targets []TargetProbe
	Elapsed time.Duration
}

// OK reports whether every target could be scraped.
func (p Probe) OK() bool {
	for _, t := range p.Targets {
		if !t.OK() {
			return false
		}
	}

	return len(p.Targets) > 0
}
