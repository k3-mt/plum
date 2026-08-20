package server

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// Nothing to compare against is not the same as no change, and showing a flat
// arrow for it would claim a steadiness the window cannot know about.
func TestAFreshWindowHasNoTrendYet(t *testing.T) {
	var log trendLog
	if got := log.observe(14, t0); got != 0 {
		t.Errorf("first reading reported a trend of %d", got)
	}
}

// The trend is movement across the window, not the step since the last reading.
// A step would flicker: every symbol read would flash "down 1" and then settle,
// which tells you about your own last click rather than about the session.
func TestTheTrendMeasuresMovementAcrossTheWindow(t *testing.T) {
	var log trendLog
	log.observe(4, t0)
	if got := log.observe(14, t0.Add(10*time.Minute)); got != 10 {
		t.Errorf("rising: %d, want 10", got)
	}
	// Read four of them back down. Still up on where the window opened, and the
	// meter says so rather than reporting the most recent step.
	if got := log.observe(6, t0.Add(20*time.Minute)); got != 2 {
		t.Errorf("after reading some back: %d, want 2 — up 2 on where it started", got)
	}
}

func TestAFallingDebtReportsANegativeTrend(t *testing.T) {
	var log trendLog
	log.observe(20, t0)
	if got := log.observe(6, t0.Add(10*time.Minute)); got != -14 {
		t.Errorf("%d, want -14 — an hour of reading has to show as progress", got)
	}
}

// The baseline is the last reading before the window opened, not the first
// reading inside it: what the number is now is a change *from* where it stood
// half an hour ago, which is a sample taken before that point.
func TestTheBaselineIsWhereTheMeterStoodWhenTheWindowOpened(t *testing.T) {
	var log trendLog
	log.observe(10, t0)
	log.observe(20, t0.Add(10*time.Minute))
	if got := log.observe(30, t0.Add(60*time.Minute)); got != 10 {
		t.Errorf("%d, want 30 - 20: half an hour ago the meter read 20", got)
	}
}

// A window left open overnight must not fill its own history with the same
// number and push the readings that mattered out of it.
func TestAStillMeterDoesNotFillTheLog(t *testing.T) {
	var log trendLog
	for i := 0; i < 5000; i++ {
		log.observe(7, t0.Add(time.Duration(i)*time.Second))
	}
	if n := len(log.samples); n != 1 {
		t.Errorf("samples = %d, want the one reading that ever happened", n)
	}
}

func TestTheLogIsBoundedEvenWhenTheMeterNeverSettles(t *testing.T) {
	var log trendLog
	for i := 0; i < trendSamples*3; i++ {
		log.observe(i, t0.Add(time.Duration(i)*time.Millisecond))
	}
	if n := len(log.samples); n > trendSamples {
		t.Errorf("samples = %d, over the %d cap", n, trendSamples)
	}
}
