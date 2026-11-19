package main

import "strings"

// Pick is the decision half of the scoring system (spec §C5).
//
// It returns an auth id only when the winner is clearly ahead; otherwise it
// declines, so the host's own affinity and round-robin stay in charge. Declining
// is always safe: CLIProxyAPI treats Handled=false as "use the built-in
// scheduler", which is exactly the behaviour we want when the plugin has nothing
// better to say.
type candidate struct {
	AuthID      string
	Quota       Quota
	SameSession bool
	// VetoReason is set when the credential itself is unusable (bad key), which
	// is not expressible through quota windows alone.
	VetoReason string
	// Suspect is set when the key is quarantined by a failed probe: the
	// user-specified -20000, below every veto, unrecoverable by stickiness.
	Suspect string
}

// scoreEntry is one candidate's score exactly as the decision computed it, so
// the audit log can never disagree with the numbers that chose the winner.
type scoreEntry struct {
	AuthID string
	Score  int
	Reason string
}

type decision struct {
	AuthID  string
	Handled bool
	Reason  string
	// Vetoed lists the auth IDs excluded by a veto, so a decline can still tell
	// the host which credentials must not serve this request.
	Vetoed []string
	// Ranked is the score list the decision used, winner first.
	Ranked []scoreEntry
}

// Pick chooses among candidates by score, declining on ties and when every
// candidate is vetoed.
type scored struct {
	authID string
	score  int
	reason string
}

func Pick(cands []candidate, opts scoreOptions) decision {
	if len(cands) == 0 {
		return decision{Reason: "no candidates"}
	}

	ranked := make([]scored, 0, len(cands))
	vetoed := 0
	for _, c := range cands {
		s, why := Score(c.Quota, c.SameSession, opts)
		if c.Suspect != "" {
			s, why = suspectPenalty, "suspect: "+c.Suspect
		} else if c.VetoReason != "" {
			s = vetoPenalty
			if c.SameSession {
				s += sessionBonus
			}
			why = c.VetoReason
		}
		if why != "" {
			vetoed++
		}
		ranked = append(ranked, scored{authID: c.AuthID, score: s, reason: why})
	}

	// Every candidate is out of quota: say nothing and let the host apply its
	// normal error/cooldown path rather than inventing a rejection.
	if vetoed == len(ranked) {
		return decision{Reason: "all candidates vetoed", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
	}

	if len(ranked) == 1 {
		if ranked[0].reason != "" {
			return decision{Reason: "only candidate vetoed", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
		}
		return decision{AuthID: ranked[0].authID, Handled: true, Reason: "single candidate",
			Ranked: rankedEntries(ranked)}
	}

	best, second := ranked[0], scored{score: -1 << 30}
	for _, s := range ranked[1:] {
		switch {
		case s.score > best.score:
			second = best
			best = s
		case s.score > second.score:
			second = s
		}
	}
	if float64(best.score-second.score) < tieEpsilon {
		return decision{Reason: "tie, deferring to host", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
	}
	if best.reason != "" {
		return decision{Reason: "winner vetoed", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
	}
	return decision{AuthID: best.authID, Handled: true, Reason: "highest score", Ranked: rankedEntries(ranked)}
}

// rankedEntries converts the working scores into the decision's audit form.
func rankedEntries(ranked []scored) []scoreEntry {
	out := make([]scoreEntry, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, scoreEntry{AuthID: s.authID, Score: s.score, Reason: s.reason})
	}
	return out
}

// vetoedIDs extracts the ids of every vetoed candidate, in candidate order.
func vetoedIDs(ranked []scored) []string {
	var out []string
	for _, s := range ranked {
		if s.reason != "" {
			out = append(out, s.authID)
		}
	}
	return out
}

// sessionKeyFromHeaders derives the conversation identity the host uses for
// affinity, in its documented precedence order. Empty means "no session", in
// which case no candidate earns the stickiness bonus.
func sessionKeyFromHeaders(headers map[string][]string) string {
	for _, name := range []string{
		"x-opencode-session",
		"x-session-id",
		"x-claude-code-session-id",
		"x-codex-session-id",
	} {
		for k, values := range headers {
			if !strings.EqualFold(k, name) || len(values) == 0 {
				continue
			}
			if v := strings.TrimSpace(values[0]); v != "" {
				return v
			}
		}
	}
	return ""
}
