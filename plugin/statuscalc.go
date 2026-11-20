package main

import (
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// The status-page arithmetic: everything the panel shows beyond the raw quota
// snapshot. Kept out of management.go so the route logic and the maths it serves
// stay separately readable.

// breakdownFor explains one score in the terms Score computed it.
//
// #FUNC     Turn a quota snapshot into the score terms the page renders.
// #IN       the snapshot, the options Score would use, and the veto reason.
// #OUT      rolling share (0..700), density share (0..200), and which term
//
//	replaced the sum (veto, quarantine, or nothing).
//
// #WHY      a score shown without its terms is a black box: "why 608?" was the
//
//	panel's most common question and the log was the only answer.
func breakdownFor(q Quota, opts scoreOptions, veto string) *scoreBreakdown {
	rolling := neutralFraction
	if q.Rolling.Known {
		rolling = q.Rolling.Remaining() / 100
	}
	out := &scoreBreakdown{
		Rolling: int(rollingWeight * rolling),
		Density: int(monthlyWeight * monthlyDensity(q.Monthly, opts)),
		Neutral: !q.Rolling.Known && !q.Monthly.Known,
	}
	if veto != "" {
		out.Veto = veto
	}
	return out
}

// countdown names the nearest window reset in human terms.
//
// #FUNC     The "resets in 3h12m" line.
// #WHY      the raw RFC3339 timestamp answers "when" only after mental
//
//	timezone arithmetic; the countdown answers it directly.
func countdown(q Quota) string {
	nearest := time.Time{}
	for _, w := range []Window{q.Rolling, q.Weekly, q.Monthly} {
		if w.Known && w.ResetsAt.After(time.Now()) && (nearest.IsZero() || w.ResetsAt.Before(nearest)) {
			nearest = w.ResetsAt
		}
	}
	if nearest.IsZero() {
		return ""
	}
	return humanDuration(time.Until(nearest))
}

// burnHistory remembers the last two rolling-window samples per auth id, which
// is all the estimation below needs and all it can honestly use.
var (
	burnMu   sync.Mutex
	burnSeen = map[string][]burnSample{}
)

type burnSample struct {
	at       time.Time
	usedFrac float64 // rolling window used fraction 0..1
}

// burnEstimate projects how long the rolling window lasts at the observed pace.
//
// #FUNC     Estimate "empty in Nh Nm" from the last two samples.
// #IN       the auth id and the fresh snapshot.
// #OUT      the spend rate per hour (0..1/h) and the time-to-empty; nil/"" when
//
//	fewer than two samples exist — no data is not a flat line.
//
// #EDGE-OUT one sample only → no estimate: a single point has no slope.
// #WHY      "remaining 12%" is unactionable without "and you burn 4%/h";
//
//	inventing a slope from one sample would be a confident guess.
func burnEstimate(authID string, q Quota) (*float64, string) {
	if !q.Rolling.Known {
		return nil, ""
	}
	used := q.Rolling.Percent / 100
	now := time.Now()

	burnMu.Lock()
	samples := append(burnSeen[authID], burnSample{at: now, usedFrac: used})
	// Keep the last two samples within the window's own horizon.
	if len(samples) > 2 {
		samples = samples[len(samples)-2:]
	}
	burnSeen[authID] = samples
	burnMu.Unlock()

	if len(samples) < 2 {
		return nil, ""
	}
	dt := samples[1].at.Sub(samples[0].at).Hours()
	if dt < 0.016 { // under a minute: the slope is noise
		return nil, ""
	}
	slope := (samples[1].usedFrac - samples[0].usedFrac) / dt
	if slope <= 0 {
		// Not burning: the honest answer is "not shrinking", not a big number.
		zero := 0.0
		return &zero, ""
	}
	remainingFrac := 1 - used
	emptyIn := time.Duration(remainingFrac / slope * float64(time.Hour))
	return &slope, humanDuration(emptyIn)
}

// humanDuration renders a duration as "3h12m", "45s" or "2d 4h".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	round := d.Round(time.Second)
	switch {
	case round < time.Minute:
		return fmt.Sprintf("%ds", int(round.Seconds()))
	case round < time.Hour:
		return fmt.Sprintf("%dm", int(round.Minutes()))
	case round < 24*time.Hour:
		h := int(round.Hours())
		return fmt.Sprintf("%dh%dm", h, int(round.Minutes())-h*60)
	default:
		days := int(round.Hours()) / 24
		return fmt.Sprintf("%dd%dh", days, int(round.Hours())-days*24)
	}
}

// hostVersionBuiltFor is the host module version this plugin compiled against.
//
// #FUNC     Report the CLIProxyAPI version the plugin was built for.
// #OUT      the module requirement (e.g. "v8.0.77"), or "" outside a module build.
// #WHY      an ABI mismatch shows up as "the panel's buttons do nothing" long
//
//	before anyone reads a version line; naming the built-for version
//	makes the first suspicion the right one.
func hostVersionBuiltFor() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if strings.HasPrefix(dep.Path, "github.com/router-for-me/CLIProxyAPI/") {
			return dep.Version
		}
	}
	return ""
}
