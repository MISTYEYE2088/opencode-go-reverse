package main

import (
	"math"
	"time"
)

// Scoring for the optional quota-aware scheduler (spec §C5).
//
// Rules, as specified:
//   - the 5-hour window dominates: 700 points
//   - the monthly window is 200 points, but scored by *density* (remaining quota
//     per remaining time), not by raw remaining percentage
//   - those two together can never open a gap larger than 1000 (700+200 sums to
//     900, so the cap is satisfied structurally, with 100 points of headroom)
//   - the same session header on a candidate: +1000
//   - 5-hour or weekly window exhausted, or monthly above the veto threshold:
//     -10000
//
// Why density for monthly: a monthly window is a budget with an expiry date.
// 15 % left with 3 days to reset is 5 %/day of spending room; 50 % left with 20
// days is 2.5 %/day. The first account can absorb more traffic now and its quota
// is use-it-or-lose-it, so it must outrank the second even though its raw
// remaining percentage is lower.
//
// Density is expressed as a *pace ratio* against an even-spend schedule:
//
//	pace = remaining_fraction / remaining_time_fraction
//
// pace = 1 means exactly on schedule (spending evenly across the period);
// pace > 1 means ahead of schedule with room to take more; pace < 1 means burning
// faster than the period allows. The ratio is capped and rescaled into 0..200.

const (
	// rollingWeight is the share of the score owned by the 5-hour window.
	rollingWeight = 700.0
	// monthlyWeight is the share owned by the density-scored monthly window.
	monthlyWeight = 200.0
	// sessionBonus is awarded to the candidate already bound to this session.
	sessionBonus = 1000
	// vetoPenalty removes a candidate that has no quota left.
	vetoPenalty = -10000
	// neutralFraction is used when a window cannot be read: the midpoint of its
	// share, so an unreadable account is neither starved nor privileged.
	neutralFraction = 0.5
	// tieEpsilon: a win smaller than this is not a decision worth making.
	tieEpsilon = 1.0
	// defaultVetoMonthlyPercent vetoes a candidate above this monthly usage.
	defaultVetoMonthlyPercent = 95.0
	// defaultMonthlyWindow is the assumed billing period when the reset time is
	// known but the period length is not published.
	defaultMonthlyWindow = 30 * 24 * time.Hour
	// defaultPaceCap is the pace ratio that earns full monthly points.
	defaultPaceCap = 2.0
	// minTimeFraction floors the remaining-time fraction so an account about to
	// reset cannot divide by ~0.
	minTimeFraction = 0.02
)

// Window is one quota window as reported by /usage. Known=false means the read
// failed or the field was missing — never treat that as zero.
type Window struct {
	Percent  float64
	Status   string // "ok" or "rate-limited"
	ResetsAt time.Time
	Known    bool
}

// Exhausted reports whether this window has nothing left.
// #FUNC      Report whether this window has no quota left.
// #EDGE-IN   `rate-limited` with percent under 100 → exhausted anyway: the
//
//	gateway's status word outranks its counter → TestVetoRules
//
// #EDGE-IN   an unreadable window → never exhausted: no data is not zero
// #WHY       `Exhausted` is the veto source; a silent false would route traffic
//
//	onto a throttled account, a silent true would sideline a good one.
func (w Window) Exhausted() bool {
	if !w.Known {
		return false
	}
	return w.Status == "rate-limited" || w.Percent >= 100
}

// Remaining is the complement of Percent, clamped to 0..100.
//
// #FUNC      The percent of a window still available.
// #WHY       clamped `math.Min(100, ...)` because the gateway has been seen to
//
//	report percent above 100 on a hard-throttled key.
func (w Window) Remaining() float64 {
	if !w.Known {
		return 0
	}
	return math.Max(0, math.Min(100, 100-w.Percent))
}

// Quota is one account's snapshot. Any window may be unknown.
type Quota struct {
	Rolling Window // 5-hour
	Weekly  Window
	Monthly Window
}

// Known reports whether any window could be read.
func (q Quota) Known() bool {
	return q.Rolling.Known || q.Weekly.Known || q.Monthly.Known
}

// scoreOptions carries the knobs and the clock.
type scoreOptions struct {
	VetoMonthlyPercent float64
	MonthlyWindow      time.Duration
	PaceCap            float64
	Now                time.Time
	// FreeModel marks a request for a model that draws no paid quota (the
	// upstream "-free" tier). Window vetoes protect PAID quota, so they do not
	// apply; the windows still feed the score, and the -20000 dead-key
	// quarantine is model-independent.
	FreeModel bool
	// TieSeed makes an equal-score decision deterministic. It is the session
	// hash, so one conversation always lands on one account while different
	// conversations spread across the pool.
	TieSeed string
}

func defaultScoreOptions(now time.Time) scoreOptions {
	return scoreOptions{
		VetoMonthlyPercent: defaultVetoMonthlyPercent,
		MonthlyWindow:      defaultMonthlyWindow,
		PaceCap:            defaultPaceCap,
		Now:                now,
	}
}

// monthlyDensity returns 0..1: monthly spending room left per unit of remaining
// time, relative to an even-spend pace.
//
// #FUNC      Score the monthly window by burn pace, not raw percentage.
// #IN        20% left, reset in 2 days of a 30-day window → full density.
// #EDGE-IN   no `resetsAt` → raw remaining/100 → TestNoResetTimeFallsBackToRawRemaining
// #EDGE-IN   reset far in the past → raw remaining: an implausible clock must
//
//	not grant full density → TestMonthlyDensityIgnoresAncientResetsAt
//
// #EDGE-IN   reset overdue within the hour → density 1: the quota is about to
//
//	renew, spend it → TestResettingNowIsTreatedAsFullDensity
//
// #OUT       `pace/cap` with time fraction floored at `minTimeFraction`.
// #WHY       `pace` compares a budget against its own clock: 15% left with 3
//
//	days to reset can absorb more traffic now than 50% left with 20
//	days, and use-it-or-lose-it quota must be spent before it expires.
func monthlyDensity(w Window, opts scoreOptions) float64 {
	if !w.Known {
		return neutralFraction
	}
	remaining := w.Remaining()
	if remaining <= 0 {
		return 0
	}
	if w.ResetsAt.IsZero() || opts.MonthlyWindow <= 0 {
		return remaining / 100
	}
	left := w.ResetsAt.Sub(opts.Now)
	if left <= 0 {
		if left > -time.Hour {
			// Reset is due; whatever is left is about to be renewed.
			return 1
		}
		// A reset timestamp far in the past is not "due", it is implausible
		// data: fall back to raw remaining instead of granting full density.
		return remaining / 100
	}
	timeFraction := left.Seconds() / opts.MonthlyWindow.Seconds()
	if timeFraction > 1 {
		timeFraction = 1
	}
	if timeFraction < minTimeFraction {
		timeFraction = minTimeFraction
	}
	pace := (remaining / 100) / timeFraction
	cap := opts.PaceCap
	if cap <= 0 {
		cap = defaultPaceCap
	}
	if pace > cap {
		pace = cap
	}
	return pace / cap
}

// Score returns the integer score for one candidate and the veto reason (empty
// when not vetoed).
//
// #FUNC      One candidate's rank: 700·rolling + 200·monthly density + 1000
//
//	same-session, veto at -10000, quarantined keys at -20000.
//
// #EDGE-IN   an unreadable window → the neutral half-share (450 of 900), never
//
//	zero: no data must not starve an account → TestUnknownQuotaIsNeutral
//
// #EDGE-IN   `opts.FreeModel` (a "-free" model) → vetoes are skipped: free tier
//
//	draws no paid quota and must stay reachable → TestFreeModelSkipsWindowVetoes
//
// #OUT       the integer score and the veto reason string.
// #WHY       `rollingWeight`=700 dominates because the 5-hour window is the
//
//	binding constraint at request time; monthly is density-scored
//	because raw percentage ignores when the budget expires.
func Score(q Quota, sameSession bool, opts scoreOptions) (int, string) {
	if opts.Now.IsZero() {
		opts = defaultScoreOptions(time.Now())
	}

	bonus := 0
	if sameSession {
		bonus = sessionBonus
	}

	if opts.FreeModel {
		// Free tier: score by the windows, veto nothing on their account —
		// a month at 97% used must not block a model that costs nothing.
	} else {
		// Veto first: no quota left makes the rest of the maths irrelevant.
		switch {
		case q.Rolling.Exhausted():
			return vetoPenalty + bonus, "5h-exhausted"
		case q.Weekly.Exhausted():
			return vetoPenalty + bonus, "weekly-exhausted"
		case q.Monthly.Exhausted():
			// A window the gateway already reports as rate-limited is exhausted
			// regardless of its percent, which may still sit below the threshold.
			return vetoPenalty + bonus, "monthly-exhausted"
		case opts.VetoMonthlyPercent > 0 && q.Monthly.Known && q.Monthly.Percent > opts.VetoMonthlyPercent:
			return vetoPenalty + bonus, "monthly-over-threshold"
		}
	}

	// 5-hour share: raw remaining. The window is short enough that time left is
	// nearly equal across accounts, so density there adds noise, not signal.
	rolling := neutralFraction
	if q.Rolling.Known {
		rolling = q.Rolling.Remaining() / 100
	}

	total := rollingWeight*rolling + monthlyWeight*monthlyDensity(q.Monthly, opts)
	return int(math.Round(total)) + bonus, ""
}
