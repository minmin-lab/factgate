// Package kbhnsw measures how an approximate (HNSW) retrieval arm deviates
// from the exact arm that KB-1 uses by default (kb-design §7, plan K3.4).
//
// The unit of comparison is the hit set: the chunk identities a search
// returns for one query at one k. Exact retrieval orders the collection by
// cosine distance under a full scan, so its hit set is a function of the
// query and the collection alone (up to ties at the k boundary, which are
// reported separately rather than resolved). The approximate arm may return
// a different set, or fewer than k rows when a filter is applied after the
// index walk; both are recorded as deviations.
package kbhnsw

import (
	"errors"
	"sort"
)

// Key identifies one chunk of one document version.
type Key struct {
	DocID      string `json:"doc_id"`
	Version    int    `json:"version"`
	ChunkIndex int    `json:"chunk_index"`
}

// Hit is one returned row with its cosine distance to the query.
type Hit struct {
	Key
	Distance float64 `json:"distance"`
}

// Outcome is the comparison of one query's approximate hit set against the
// exact top-k.
type Outcome struct {
	K             int     `json:"k"`
	Returned      int     `json:"returned"`        // rows the approximate arm returned
	Overlap       int     `json:"overlap"`         // |approx ∩ exact_top_k|
	Recall        float64 `json:"recall"`          // overlap / k
	Jaccard       float64 `json:"jaccard"`         // overlap / |approx ∪ exact_top_k|
	Identical     bool    `json:"identical"`       // same set and exactly k rows
	Short         bool    `json:"short"`           // fewer than k rows returned
	TieAtBoundary bool    `json:"tie_at_boundary"` // exact distance at rank k equals rank k+1
}

// Compare evaluates approx against the first k hits of exact. exact must be
// sorted by ascending distance and should hold at least k+1 rows when the
// collection allows, so that a tie at the boundary can be detected.
func Compare(exact, approx []Hit, k int) (Outcome, error) {
	if k <= 0 {
		return Outcome{}, errors.New("kbhnsw: k must be positive")
	}
	if len(exact) < k {
		return Outcome{}, errors.New("kbhnsw: exact result holds fewer than k rows")
	}
	for i := 1; i < len(exact); i++ {
		if exact[i].Distance < exact[i-1].Distance {
			return Outcome{}, errors.New("kbhnsw: exact result is not sorted by distance")
		}
	}
	top := make(map[Key]struct{}, k)
	for _, hit := range exact[:k] {
		top[hit.Key] = struct{}{}
	}
	if len(top) != k {
		return Outcome{}, errors.New("kbhnsw: exact result repeats a chunk identity")
	}
	seen := make(map[Key]struct{}, len(approx))
	overlap := 0
	for _, hit := range approx {
		if _, dup := seen[hit.Key]; dup {
			return Outcome{}, errors.New("kbhnsw: approximate result repeats a chunk identity")
		}
		seen[hit.Key] = struct{}{}
		if _, ok := top[hit.Key]; ok {
			overlap++
		}
	}
	union := k + len(approx) - overlap
	out := Outcome{
		K:        k,
		Returned: len(approx),
		Overlap:  overlap,
		Recall:   float64(overlap) / float64(k),
		Short:    len(approx) < k,
	}
	if union > 0 {
		out.Jaccard = float64(overlap) / float64(union)
	}
	out.Identical = len(approx) == k && overlap == k
	if len(exact) > k && exact[k].Distance == exact[k-1].Distance {
		out.TieAtBoundary = true
	}
	return out, nil
}

// Summary aggregates the outcomes of one configuration (k, index parameters,
// filter mode) over all queries.
type Summary struct {
	Queries        int     `json:"queries"`
	Identical      int     `json:"identical"`
	Short          int     `json:"short"`
	TiesExcluded   int     `json:"ties_excluded"` // queries with a boundary tie, excluded from identity_rate only
	IdentityRate   float64 `json:"identity_rate"` // identical / (queries - ties_excluded)
	MeanRecall     float64 `json:"mean_recall"`
	MinRecall      float64 `json:"min_recall"`
	MeanJaccard    float64 `json:"mean_jaccard"`
	RecallBelow1   int     `json:"recall_below_1"` // queries whose recall < 1
	MedianReturned int     `json:"median_returned"`
}

// Summarize folds outcomes into a Summary. Identity rate excludes queries
// whose exact ranking has a tie at the k boundary, because for those the
// exact arm itself does not define a unique set; recall and Jaccard keep
// them, since the tie can only lower those figures for the approximate arm.
func Summarize(outcomes []Outcome) Summary {
	s := Summary{Queries: len(outcomes), MinRecall: 1}
	if len(outcomes) == 0 {
		s.MinRecall = 0
		return s
	}
	var recall, jaccard float64
	returned := make([]int, 0, len(outcomes))
	for _, o := range outcomes {
		recall += o.Recall
		jaccard += o.Jaccard
		if o.Recall < s.MinRecall {
			s.MinRecall = o.Recall
		}
		if o.Recall < 1 {
			s.RecallBelow1++
		}
		if o.Short {
			s.Short++
		}
		if o.TieAtBoundary {
			s.TiesExcluded++
		} else if o.Identical {
			s.Identical++
		}
		returned = append(returned, o.Returned)
	}
	sort.Ints(returned)
	s.MedianReturned = returned[len(returned)/2]
	s.MeanRecall = recall / float64(len(outcomes))
	s.MeanJaccard = jaccard / float64(len(outcomes))
	if n := len(outcomes) - s.TiesExcluded; n > 0 {
		s.IdentityRate = float64(s.Identical) / float64(n)
	}
	return s
}
