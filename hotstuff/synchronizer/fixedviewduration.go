package synchronizer

import "time"

type fixedViewDuration struct {
	viewDuration time.Duration // the start time
}

func NewFixedViewDuration(viewDuration time.Duration) ViewDuration {
	return &fixedViewDuration{
		viewDuration: viewDuration,
	}
}

func (v *fixedViewDuration) ViewSucceeded() {
}

func (v *fixedViewDuration) ViewTimeout() {
}

func (v *fixedViewDuration) ViewStarted() {
}

func (v *fixedViewDuration) Duration() time.Duration {
	return v.viewDuration
}
