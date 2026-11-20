package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Pick is the decision half of the scoring system (spec §C5).
//
// It returns an auth id whenever one candidate is usable, breaking ties
// deterministically from the session hash so a conversation keeps its account.
// It declines only when it has nothing to say: no candidates, no usable
// candidate, or no candidate that belongs to this provider. Declining is always
// safe: CLIProxyAPI treats Handled=false as "use the built-in scheduler", which
// is what we want when the plugin has no better answer.
type candidate struct {
	AuthID      string
	Quota       Quota
	SameSession bool
	// Suspect is set when the key is quarantined by a failed probe: the
	// user-specified -20000, below every veto, unrecoverable by stickiness.
	Suspect string
	// Foreign marks a credential from another provider that the host offered
	// for the same model id. We cannot read its quota, so it scores zero: a
	// fallback for when the subscription is spent, never a competitor.
	Foreign bool
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

// Pick chooses among candidates by score: our own credentials on their real
// quota, another provider's credentials at zero, and a session's bound account
// on top of everything. Ties are broken from the session hash.
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
		if c.Foreign {
			// We cannot read another provider's quota, so it contributes no
			// signal: zero, plus the stickiness bonus if this session already
			// runs on it. Any usable subscription credential scores above zero,
			// so the subscription is used first and only a spent pool lets the
			// request fall through to the other provider.
			s, why = 0, ""
			if c.SameSession {
				s = sessionBonus
			}
		} else if c.Suspect != "" {
			s, why = suspectPenalty, "suspect: "+c.Suspect
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
	if best.reason != "" {
		return decision{Reason: "winner vetoed", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
	}
	if float64(best.score-second.score) < tieEpsilon {
		// A tie is still a decision we can make deterministically. Deferring
		// left sticky_sessions at 0 and let the host rotate one conversation
		// across accounts, so prompt cache never hit.
		if winner := breakTie(ranked, best.score, opts.TieSeed); winner != "" {
			return decision{AuthID: winner, Handled: true, Reason: "tie, broken by session hash",
				Ranked: rankedEntries(ranked)}
		}
		return decision{Reason: "tie, deferring to host", Vetoed: vetoedIDs(ranked), Ranked: rankedEntries(ranked)}
	}
	return decision{AuthID: best.authID, Handled: true, Reason: "highest score", Ranked: rankedEntries(ranked)}
}

// breakTie names the winner of a tie deterministically.
//
// #FUNC     Pick one of the equal-scoring candidates from the session seed.
// #IN       the ranked scores, the tied score, and the session hash.
// #EDGE-IN  empty seed (no session header) → the ordering falls back to the
//
//	candidate id alone, so the pool still uses one stable account
//	instead of alternating.
//
// #OUT      the auth id this session must use, or "" when every tied candidate
//
//	is vetoed and the caller should keep declining.
//
// #WHY      hashing the session into the comparison keeps one conversation on
//
//	one account while different conversations still spread across the
//	pool; a fixed "lowest id wins" would collapse every session onto
//	one credential.
func breakTie(ranked []scored, top int, seed string) string {
	winner, bestSum := "", ""
	for _, s := range ranked {
		if s.score != top || s.reason != "" {
			continue
		}
		sum := sha256.Sum256([]byte(seed + "\x00" + s.authID))
		printable := hex.EncodeToString(sum[:])
		if winner == "" || printable < bestSum {
			winner, bestSum = s.authID, printable
		}
	}
	return winner
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
