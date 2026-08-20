package server

import (
	"sync"
	"time"
)

// A number on its own does not say which way it is going, and which way it is
// going is most of what a glance from across the room is for. Fourteen unmet is
// a different situation depending on whether it was four ten minutes ago or
// forty.
//
// The history is kept in memory for the life of the window and never written
// down. A trend is a live signal about what is happening right now; carrying it
// across restarts would mean a file, a format and a trimming policy in exchange
// for a number nobody reads on the morning after. A restarted window simply has
// no trend yet, and says so by not showing one.
const (
	trendWindow  = 30 * time.Minute
	trendSamples = 512 // a hard stop, far above what changing every second reaches
)

type trendLog struct {
	mu      sync.Mutex
	samples []trendSample
}

type trendSample struct {
	at    time.Time
	unmet int
}

// observe records where the meter is now and reports how far it has moved
// across the window. Positive means the debt is growing: the agent is ahead of
// you. It returns 0 when there is nothing to compare against yet, which reads
// correctly as "no trend" rather than as "no change".
func (t *trendLog) observe(unmet int, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Only movement is worth a sample. A window sitting still for an hour must
	// not fill the log with the same number and push its own history out.
	if n := len(t.samples); n == 0 || t.samples[n-1].unmet != unmet {
		t.samples = append(t.samples, trendSample{at: now, unmet: unmet})
		if len(t.samples) > trendSamples {
			t.samples = t.samples[len(t.samples)-trendSamples:]
		}
	}

	cutoff := now.Add(-trendWindow)
	// The oldest sample still inside the window is the baseline. Samples older
	// than that are dropped, but only after one has been kept: the last reading
	// before the window opened is what the current number is a change *from*.
	keep := 0
	for i, s := range t.samples {
		if s.at.After(cutoff) {
			break
		}
		keep = i
	}
	if keep > 0 {
		t.samples = t.samples[keep:]
	}
	if len(t.samples) < 2 {
		return 0
	}
	return unmet - t.samples[0].unmet
}
