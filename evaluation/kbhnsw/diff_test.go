package kbhnsw

import (
	"math"
	"testing"
)

func key(i int) Key { return Key{DocID: "D", Version: 1, ChunkIndex: i} }

func hits(dist ...float64) []Hit {
	out := make([]Hit, 0, len(dist))
	for i, d := range dist {
		out = append(out, Hit{Key: key(i), Distance: d})
	}
	return out
}

func TestCompareIdentical(t *testing.T) {
	exact := hits(0.1, 0.2, 0.3, 0.4)
	out, err := Compare(exact, exact[:3], 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Identical || out.Recall != 1 || out.Jaccard != 1 || out.Short || out.TieAtBoundary {
		t.Fatalf("unexpected outcome %+v", out)
	}
}

func TestComparePartialAndShort(t *testing.T) {
	exact := hits(0.1, 0.2, 0.3, 0.4)
	approx := []Hit{{Key: key(0), Distance: 0.1}, {Key: key(3), Distance: 0.4}}
	out, err := Compare(exact, approx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.Identical || !out.Short || out.Overlap != 1 {
		t.Fatalf("unexpected outcome %+v", out)
	}
	if math.Abs(out.Recall-1.0/3) > 1e-12 || math.Abs(out.Jaccard-1.0/4) > 1e-12 {
		t.Fatalf("recall/jaccard %+v", out)
	}
}

func TestCompareTieAtBoundary(t *testing.T) {
	exact := hits(0.1, 0.2, 0.3, 0.3)
	out, err := Compare(exact, exact[:3], 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TieAtBoundary || !out.Identical {
		t.Fatalf("unexpected outcome %+v", out)
	}
	// A tie strictly inside the top-k is not a boundary tie.
	out, err = Compare(hits(0.1, 0.1, 0.3, 0.4), hits(0.1, 0.1, 0.3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.TieAtBoundary {
		t.Fatalf("inner tie reported as boundary tie %+v", out)
	}
}

func TestCompareRejectsBadInput(t *testing.T) {
	if _, err := Compare(hits(0.1, 0.2), nil, 3); err == nil {
		t.Fatal("short exact accepted")
	}
	if _, err := Compare(hits(0.2, 0.1, 0.3), nil, 2); err == nil {
		t.Fatal("unsorted exact accepted")
	}
	dup := []Hit{{Key: key(0), Distance: 0.1}, {Key: key(0), Distance: 0.2}}
	if _, err := Compare(hits(0.1, 0.2, 0.3), dup, 2); err == nil {
		t.Fatal("duplicate approximate identity accepted")
	}
	if _, err := Compare(hits(0.1, 0.2, 0.3), nil, 0); err == nil {
		t.Fatal("k=0 accepted")
	}
}

func TestSummarize(t *testing.T) {
	outcomes := []Outcome{
		{K: 3, Returned: 3, Overlap: 3, Recall: 1, Jaccard: 1, Identical: true},
		{K: 3, Returned: 3, Overlap: 2, Recall: 2.0 / 3, Jaccard: 0.5},
		{K: 3, Returned: 2, Overlap: 2, Recall: 2.0 / 3, Jaccard: 2.0 / 3, Short: true},
		{K: 3, Returned: 3, Overlap: 3, Recall: 1, Jaccard: 1, Identical: true, TieAtBoundary: true},
	}
	s := Summarize(outcomes)
	if s.Queries != 4 || s.Identical != 1 || s.Short != 1 || s.TiesExcluded != 1 || s.RecallBelow1 != 2 {
		t.Fatalf("counts %+v", s)
	}
	if math.Abs(s.IdentityRate-1.0/3) > 1e-12 {
		t.Fatalf("identity rate %+v", s)
	}
	if math.Abs(s.MeanRecall-(1+2.0/3+2.0/3+1)/4) > 1e-12 || math.Abs(s.MinRecall-2.0/3) > 1e-12 {
		t.Fatalf("recall %+v", s)
	}
	if s.MedianReturned != 3 {
		t.Fatalf("median returned %+v", s)
	}
	if e := Summarize(nil); e.Queries != 0 || e.MinRecall != 0 || e.IdentityRate != 0 {
		t.Fatalf("empty summary %+v", e)
	}
}
