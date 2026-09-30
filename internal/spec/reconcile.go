package spec

// Match tiers (design §5.4). "pinned" is stored as match = 'anchored'.
const (
	TierPinned  = "pinned"
	TierPath    = "path"
	TierSlug    = "slug"
	TierSimilar = "similar"
	TierNew     = "new"
)

// Similarity thresholds for tier 4.
const (
	SimilarMin    = 0.75
	SimilarMargin = 0.15
)

// Prior is one section of the previous head version, as the store has it.
type Prior struct {
	Key      string // anchor key when the section is pinned, else ""
	Kind     string // KindRoot | KindHeading | KindBlock; "" is treated as heading
	PathSlug string
	OwnSlug  string
	Heading  string
	Body     string
	BodySHA  string
	Pinned   bool // an anchor row exists: notes or links hang off it
	Number   string
}

// Match says where one section of the new document came from.
type Match struct {
	Ordinal    int     // section ordinal in the new Doc
	Tier       string  // Tier*
	Prior      int     // index into the prior slice; -1 for TierNew
	PriorKey   string  // Prior.Key of the match, if pinned
	Confidence float64 // similarity for TierSimilar; 1 for other matches; 0 for new
}

// ReportLine is one entry of the reconciliation report.
type ReportLine struct {
	Ordinal       int // new section ordinal; -1 for orphaned/removed
	Prior         int // prior index; -1 for new
	Tier          string
	Key           string // anchor key, when there is one
	PathSlug      string
	PriorPathSlug string
	Heading       string
	PriorHeading  string
	Number        string
	PriorNumber   string
	Confidence    float64
	Pinned        bool // the prior carries durable state
}

// Report is the outcome of Reconcile. Matches has one entry per new section,
// in ordinal order; the other lists are the human-facing summary.
type Report struct {
	Matches []Match
	// Renamed: a pinned section whose path changed (tier 1), or a tier-4
	// similarity match (reported with its confidence).
	Renamed []ReportLine
	// Moved: a tier-3 match — same own slug, different path.
	Moved []ReportLine
	New   []ReportLine
	// Orphaned: pinned priors with no match; the anchor must be kept.
	Orphaned []ReportLine
	// Removed: un-pinned priors with no match; nothing pointed at them.
	Removed []ReportLine
	// PinDropped: pinned priors matched only at tier 2–4 because their pin
	// comment is gone. Durable state must not follow such a match silently:
	// the caller re-pins or asks for confirmation.
	PinDropped []ReportLine
}

// Reconcile matches the sections of next against prior (design §5.4). Tiers
// run in order over everything still unmatched; within a tier a match is
// accepted only when it is unique on both sides. Splits and merges are never
// detected: at most one new section claims each prior.
func Reconcile(prior []Prior, next *Doc) *Report {
	n := len(next.Sections)
	matchOf := make([]int, n) // new ordinal -> prior index
	tierOf := make([]string, n)
	conf := make([]float64, n)
	for i := range matchOf {
		matchOf[i] = -1
	}
	taken := make([]bool, len(prior))
	take := func(ord, p int, tier string, c float64) {
		matchOf[ord], tierOf[ord], conf[ord] = p, tier, c
		taken[p] = true
	}
	kindOf := func(p *Prior) string {
		if p.Kind == "" {
			return KindHeading
		}
		return p.Kind
	}
	sameKind := func(s *Section, p *Prior) bool {
		return kindOf(p) == s.Kind
	}

	// Tier 1: pin comment / {#key} / block key known to the prior version.
	byKey := map[string]int{}
	for i := range prior {
		if prior[i].Key != "" {
			byKey[prior[i].Key] = i
		}
	}
	for o := range next.Sections {
		if k := next.Sections[o].PinKey; k != "" {
			if p, ok := byKey[k]; ok && !taken[p] {
				take(o, p, TierPinned, 1)
			}
		}
	}

	// Tiers 2 and 3: an exact key, unique on both sides among the unmatched.
	uniqueTier := func(tier string, key func(s *Section) string, pkey func(p *Prior) string) {
		ps := map[string][]int{}
		for i := range prior {
			if !taken[i] {
				ps[pkey(&prior[i])] = append(ps[pkey(&prior[i])], i)
			}
		}
		ss := map[string][]int{}
		for o := range next.Sections {
			if matchOf[o] < 0 {
				ss[key(&next.Sections[o])] = append(ss[key(&next.Sections[o])], o)
			}
		}
		for o := range next.Sections {
			if matchOf[o] >= 0 {
				continue
			}
			k := key(&next.Sections[o])
			if k == "" || len(ss[k]) != 1 || len(ps[k]) != 1 {
				continue
			}
			if p := ps[k][0]; sameKind(&next.Sections[o], &prior[p]) {
				take(o, p, tier, 1)
			}
		}
	}
	uniqueTier(TierPath, func(s *Section) string { return s.PathSlug }, func(p *Prior) string { return p.PathSlug })
	uniqueTier(TierSlug, func(s *Section) string {
		if s.Kind == KindRoot {
			return ""
		}
		return s.OwnSlug
	}, func(p *Prior) string { return p.OwnSlug })

	// Tier 4: body similarity, best ≥ SimilarMin and beating the runner-up
	// by ≥ SimilarMargin in both directions. Empty bodies never match: bare
	// container headings would all look identical.
	var sOpen, pOpen []int
	sNorm, pNorm := map[int]string{}, map[int]string{}
	for o := range next.Sections {
		if matchOf[o] < 0 && next.Sections[o].Kind != KindRoot {
			if b := NormalizeBody(next.Sections[o].Body); b != "" {
				sOpen = append(sOpen, o)
				sNorm[o] = b
			}
		}
	}
	for i := range prior {
		if !taken[i] && kindOf(&prior[i]) != KindRoot {
			if b := NormalizeBody(prior[i].Body); b != "" {
				pOpen = append(pOpen, i)
				pNorm[i] = b
			}
		}
	}
	score := map[[2]int]float64{}
	for _, o := range sOpen {
		for _, p := range pOpen {
			if sameKind(&next.Sections[o], &prior[p]) {
				score[[2]int{o, p}] = Similarity(pNorm[p], sNorm[o])
			}
		}
	}
	decisive := func(best, second float64) bool {
		return best >= SimilarMin && best-second >= SimilarMargin
	}
	for _, o := range sOpen {
		bp, best, second := -1, 0.0, 0.0
		for _, p := range pOpen {
			sc, ok := score[[2]int{o, p}]
			if !ok {
				continue
			}
			if sc > best {
				bp, best, second = p, sc, best
			} else if sc > second {
				second = sc
			}
		}
		if bp < 0 || !decisive(best, second) {
			continue
		}
		// The prior must pick this section back just as decisively.
		rival := 0.0
		for _, o2 := range sOpen {
			if o2 != o {
				if sc := score[[2]int{o2, bp}]; sc > rival {
					rival = sc
				}
			}
		}
		if decisive(best, rival) {
			take(o, bp, TierSimilar, best)
		}
	}
	// A prior claimed twice cannot happen: mutual decisiveness makes each
	// accepted pair the unique best of both its row and its column.

	r := &Report{}
	line := func(o, p int) ReportLine {
		l := ReportLine{Ordinal: o, Prior: p}
		if o >= 0 {
			s := &next.Sections[o]
			l.PathSlug, l.Heading, l.Number, l.Key = s.PathSlug, s.Heading, s.Number, s.PinKey
			l.Tier, l.Confidence = TierNew, 0
			if matchOf[o] >= 0 {
				l.Tier, l.Confidence = tierOf[o], conf[o]
			}
		}
		if p >= 0 {
			pr := &prior[p]
			l.PriorPathSlug, l.PriorHeading, l.PriorNumber, l.Pinned = pr.PathSlug, pr.Heading, pr.Number, pr.Pinned
			if l.Key == "" {
				l.Key = pr.Key
			}
		}
		return l
	}
	for o := range next.Sections {
		p := matchOf[o]
		m := Match{Ordinal: o, Tier: TierNew, Prior: p}
		if p >= 0 {
			m.Tier, m.Confidence, m.PriorKey = tierOf[o], conf[o], prior[p].Key
		}
		r.Matches = append(r.Matches, m)
		switch m.Tier {
		case TierNew:
			r.New = append(r.New, line(o, -1))
		case TierPinned:
			if prior[p].PathSlug != next.Sections[o].PathSlug {
				r.Renamed = append(r.Renamed, line(o, p))
			}
		case TierSlug:
			r.Moved = append(r.Moved, line(o, p))
		case TierSimilar:
			r.Renamed = append(r.Renamed, line(o, p))
		}
		if p >= 0 && m.Tier != TierPinned && prior[p].Pinned {
			r.PinDropped = append(r.PinDropped, line(o, p))
		}
	}
	for p := range prior {
		if taken[p] {
			continue
		}
		if prior[p].Pinned {
			r.Orphaned = append(r.Orphaned, line(-1, p))
		} else {
			r.Removed = append(r.Removed, line(-1, p))
		}
	}
	return r
}

// PriorsOf converts a parsed document into priors, for reconciling one
// revision against another without a database (tests, `spec diff`). Every
// section with a PinKey is treated as pinned.
func PriorsOf(d *Doc) []Prior {
	out := make([]Prior, len(d.Sections))
	for i := range d.Sections {
		s := &d.Sections[i]
		out[i] = Prior{Key: s.PinKey, Kind: s.Kind, PathSlug: s.PathSlug, OwnSlug: s.OwnSlug,
			Heading: s.Heading, Body: s.Body, BodySHA: s.BodySHA, Pinned: s.PinKey != "", Number: s.Number}
	}
	return out
}
