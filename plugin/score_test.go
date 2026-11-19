package main

import (
	"testing"
	"time"
)

// sched-1-7: monthly rate-limited is a veto even below the threshold.
func TestMonthlyRateLimitedVetoed(t *testing.T) {
	q := Quota{Rolling: win(10, time.Hour), Weekly: win(10, 24*time.Hour),
		Monthly: Window{Percent: 60, Status: "rate-limited", Known: true}}
	if _, why := Score(q, false, defaultScoreOptions(now)); why == "" {
		t.Fatal("monthly rate-limited at 60% must veto")
	}
}

// inputs-1-8: an implausibly old reset time no longer earns full density.
func TestMonthlyDensityIgnoresAncientResetsAt(t *testing.T) {
	w := Window{Percent: 60, Status: "ok", Known: true, ResetsAt: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)}
	got := monthlyDensity(w, defaultScoreOptions(now))
	if got != 0.4 {
		t.Fatalf("ancient resetsAt must fall back to raw remaining (0.4), got %v", got)
	}
}

func TestRollingOwnsSevenHundredAndMonthlyTwoHundred(t *testing.T) {
	// Same monthly, 100 points of rolling difference -> ~700 points.
	a := score(t, acct(0, 40, 20*24*time.Hour), false)
	b := score(t, acct(99, 40, 20*24*time.Hour), false)
	if gap := a - b; gap < 690 || gap > 700 {
		t.Fatalf("rolling swing of 99pp moved the score by %d, want ~700", gap)
	}

	// Same rolling, same reset time, huge monthly difference -> at most 200.
	c := score(t, acct(40, 0, 20*24*time.Hour), false)
	d := score(t, acct(40, 94, 20*24*time.Hour), false)
	if gap := c - d; gap < 0 || gap > monthlyWeight {
		t.Fatalf("monthly swing moved the score by %d, want <= %.0f", gap, monthlyWeight)
	}
}

func TestGapNeverExceedsAThousand(t *testing.T) {
	best := score(t, acct(0, 0, 20*24*time.Hour), false)
	worst := score(t, acct(99.9, 94, 20*24*time.Hour), false)
	if gap := best - worst; gap > 1000 {
		t.Fatalf("quota-only gap = %d, must be <= 1000", gap)
	}
	// The monthly share cannot reach 0 or 200 at every point in the period: with
	// 20 days left an even-spend account sits at 67 % remaining, so the density
	// term tops out at 75 % of its share. 800 therefore bounds the low end.
	if gap := best - worst; gap < 800 {
		t.Fatalf("gap = %d, expected between 800 and 1000", gap)
	}
}

func TestMonthlyDensityBeatsRawRemaining(t *testing.T) {
	// Same 20 % left. The one resetting in 2 days can spend 10 %/day; the one
	// resetting in 25 days can spend 0.8 %/day.
	soon := score(t, acct(40, 80, 2*24*time.Hour), false)
	far := score(t, acct(40, 80, 25*24*time.Hour), false)
	if soon <= far {
		t.Fatalf("expiring quota must outrank slow quota: %d vs %d", soon, far)
	}
	if gap := soon - far; gap < 100 {
		t.Fatalf("density should move the score meaningfully, gap = %d", gap)
	}
}

func TestOnPaceScoresHalfOfMonthlyShare(t *testing.T) {
	// 50 % left with half the window left == exactly on schedule == pace 1.
	q := Quota{Rolling: win(50, time.Hour), Monthly: win(50, 15*24*time.Hour)}
	s := score(t, q, false)
	rolling := rollingWeight * 0.5
	monthly := monthlyWeight * 0.5
	if want := int(rolling + monthly); s != want {
		t.Fatalf("on-pace score = %d, want %d", s, want)
	}
}

func TestNoResetTimeFallsBackToRawRemaining(t *testing.T) {
	q := Quota{
		Rolling: win(50, time.Hour),
		Monthly: Window{Percent: 25, Status: "ok", Known: true}, // no ResetsAt
	}
	s := score(t, q, false)
	want := int(rollingWeight*0.5 + monthlyWeight*0.75)
	if s != want {
		t.Fatalf("no-reset fallback = %d, want %d", s, want)
	}
}

func TestStickinessOutranksHealthyDifference(t *testing.T) {
	sticky := score(t, acct(40, 40, 20*24*time.Hour), true)
	free := score(t, acct(0, 0, 20*24*time.Hour), false)
	if sticky <= free {
		t.Fatalf("stickiness (+1000) must beat a max healthy difference (900): %d vs %d", sticky, free)
	}
}

func TestVetoWinsOverStickiness(t *testing.T) {
	v, why := Score(acct(40, 96, 20*24*time.Hour), true, opts())
	if why == "" {
		t.Fatal("monthly 96 must veto")
	}
	healthy := score(t, acct(99, 40, 20*24*time.Hour), false)
	if v >= healthy {
		t.Fatalf("vetoed (%d) must lose to healthy (%d)", v, healthy)
	}
}

func TestVetoRules(t *testing.T) {
	cases := map[string]Quota{
		"5h at 100":        {Rolling: win(100, time.Hour), Weekly: win(1, time.Hour), Monthly: win(1, 10*24*time.Hour)},
		"5h rate-limited":  {Rolling: Window{Percent: 90, Status: "rate-limited", Known: true}, Weekly: win(1, time.Hour), Monthly: win(1, 10*24*time.Hour)},
		"weekly exhausted": {Rolling: win(1, time.Hour), Weekly: Window{Percent: 88, Status: "rate-limited", Known: true}, Monthly: win(1, 10*24*time.Hour)},
		"monthly over 95":  {Rolling: win(1, time.Hour), Weekly: win(1, time.Hour), Monthly: win(96, 10*24*time.Hour)},
	}
	for name, q := range cases {
		if _, why := Score(q, true, opts()); why == "" {
			t.Errorf("%s: expected veto", name)
		}
	}
	// Exactly at the threshold is not a veto.
	if _, why := Score(Quota{Rolling: win(1, time.Hour), Weekly: win(1, time.Hour), Monthly: win(95, 10*24*time.Hour)}, false, opts()); why != "" {
		t.Errorf("monthly == 95 must not veto, got %q", why)
	}
}

func TestUnknownQuotaIsNeutral(t *testing.T) {
	s, why := Score(Quota{}, false, opts())
	if why != "" {
		t.Fatalf("unknown quota must not veto: %q", why)
	}
	want := int(rollingWeight*neutralFraction + monthlyWeight*neutralFraction)
	if s != want {
		t.Fatalf("unknown quota = %d, want neutral %d", s, want)
	}
}

func TestResettingNowIsTreatedAsFullDensity(t *testing.T) {
	q := Quota{Rolling: win(10, time.Hour), Monthly: win(10, time.Second)}
	s := score(t, q, false)
	want := int(rollingWeight*0.9 + monthlyWeight*1.0)
	if s != want {
		t.Fatalf("imminent reset = %d, want %d", s, want)
	}
}
