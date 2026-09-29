package command

import (
	"sort"
	"strings"

	"github.com/kfet/acp-kit/client"
)

// Model match tiers, best first. ResolveModel only returns the models of
// the best tier any model reached.
const (
	tierExact     = iota // query is the id, byte for byte
	tierNormExact        // equal once lower-cased and stripped of - _ . space
	tierPrefix           // every query token starts at a token boundary, no gaps inside it
	tierSubseq           // letter tokens are ordered subsequences
	tierNone
)

// dateDigits is the length from which a digit run is treated as a date
// stamp (20241022): optional in an id, never satisfied by a query.
const dateDigits = 6

// ResolveModel fuzzily resolves a user's model query against the
// available ids, returning whether the query is an exact id and the
// candidates of the best tier reached, best first.
//
// The query splits on its last '/' into provider and model parts (a
// single part is matched against the model part under any provider).
// Each part is lower-cased, stripped of - _ . and spaces, and cut into
// letter runs and digit runs. A letter run matches as an ordered
// subsequence of adjacent letter runs ("anth" ~ anthropic, "opus" ~
// claude-opus); a digit run must equal or prefix one whole digit run of
// the id, where separated digits merge ("55" ~ 5-5, but not 4-5).
// Date-like digit runs are skipped, never matched.
//
// Ties inside a tier break on the tightest token span, then the
// shortest id, then the agent's order.
//
// Policy for callers: exact → switch; exactly one candidate → switch
// and echo the full id; several → list them; none → fall back to a
// plain substring filter (MatchModels does this).
func ResolveModel(all []client.ModelInfo, q string) (exact bool, cands []client.ModelInfo) {
	q = strings.TrimSpace(q)
	if q == "" {
		return false, nil
	}
	for _, m := range all {
		if m.ID == q {
			return true, []client.ModelInfo{m}
		}
	}
	qProv, qModel, hasProv := splitProvider(q)
	type scored struct {
		m          client.ModelInfo
		tier, span int
	}
	var hits []scored
	for _, m := range all {
		prov, model, _ := splitProvider(m.ID)
		tier, span := matchPart(qModel, model)
		if hasProv {
			pt, ps := matchPart(qProv, prov)
			tier, span = max(tier, pt), span+ps
		}
		if tier != tierNone {
			hits = append(hits, scored{m, tier, span})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.span != b.span {
			return a.span < b.span
		}
		return len(a.m.ID) < len(b.m.ID)
	})
	for _, h := range hits {
		if h.tier != hits[0].tier {
			break
		}
		cands = append(cands, h.m)
	}
	return false, cands
}

// splitProvider cuts s at its last '/'.
func splitProvider(s string) (prov, model string, ok bool) {
	i := strings.LastIndexByte(s, '/')
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}

// normalise lower-cases s and drops the separators - _ . and space.
func normalise(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', '.', ' ':
			return -1
		}
		return r
	}, strings.ToLower(s))
}

type token struct {
	s      string
	digits bool
	date   bool
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// tokenise cuts s into letter runs and digit runs. Separators are
// dropped; digit runs split only by separators merge (5-5 → 55) unless
// either side is date-length, which stays its own optional token.
func tokenise(s string) []token {
	var out []token
	var cur []rune
	curDigits := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		t := token{s: string(cur), digits: curDigits}
		cur = cur[:0]
		if n := len(out); t.digits && n > 0 && out[n-1].digits &&
			len(t.s) < dateDigits && !out[n-1].date {
			out[n-1].s += t.s
			out[n-1].date = len(out[n-1].s) >= dateDigits
			return
		}
		t.date = t.digits && len(t.s) >= dateDigits
		out = append(out, t)
	}
	for _, r := range strings.ToLower(s) {
		d := isDigit(r)
		if !d && !(r >= 'a' && r <= 'z') {
			flush()
			continue
		}
		if len(cur) > 0 && d != curDigits {
			flush()
		}
		curDigits = d
		cur = append(cur, r)
	}
	flush()
	return out
}

// matchPart scores one query part against one id part.
func matchPart(q, t string) (tier, span int) {
	nq, nt := normalise(q), normalise(t)
	if nq == nt {
		return tierNormExact, 0
	}
	if nq == "" {
		return tierNone, 0
	}
	qt, tt := tokenise(nq), tokenise(t)
	if len(qt) == 0 {
		return tierNone, 0
	}
	if s, ok := matchTokens(qt, tt, true); ok {
		return tierPrefix, s
	}
	if s, ok := matchTokens(qt, tt, false); ok {
		return tierSubseq, s
	}
	return tierNone, 0
}

// matchTokens finds the tightest span of target tokens that the query
// tokens match in order.
func matchTokens(qt, tt []token, strict bool) (int, bool) {
	best, found := 0, false
	for start := range tt {
		if end, ok := matchFrom(qt, tt, start, strict); ok {
			if s := end - start + 1; !found || s < best {
				best, found = s, true
			}
		}
	}
	return best, found
}

// matchFrom matches qt with qt[0] anchored at tt[i]; later query tokens
// may skip target tokens. It returns the index of the last target token
// used. Greedy earliest end per token is optimal for what follows.
func matchFrom(qt, tt []token, i int, strict bool) (int, bool) {
	end, ok := matchOne(qt[0], tt, i, strict)
	if !ok {
		return 0, false
	}
	if len(qt) == 1 {
		return end, true
	}
	for j := end + 1; j < len(tt); j++ {
		if e, ok := matchFrom(qt[1:], tt, j, strict); ok {
			return e, true
		}
	}
	return 0, false
}

// matchOne matches a single query token starting at target token i,
// returning the last target token it consumed.
func matchOne(q token, tt []token, i int, strict bool) (int, bool) {
	t := tt[i]
	if q.digits {
		return i, t.digits && !t.date && strings.HasPrefix(t.s, q.s)
	}
	if t.digits {
		return 0, false
	}
	// Letters: walk adjacent letter tokens from i.
	k := 0
	for j := i; j < len(tt) && !tt[j].digits; j++ {
		for _, c := range []byte(tt[j].s) {
			if k < len(q.s) && c == q.s[k] {
				k++
			} else if strict && k < len(q.s) {
				return 0, false
			}
		}
		if k == len(q.s) {
			return j, true
		}
	}
	return 0, false
}
