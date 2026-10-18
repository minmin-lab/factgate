package kbformal

import (
	"math/rand/v2"
	"testing"
)

func TestSweepOrderVisitsEveryKeywordOncePerCycle(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		order := sweepOrder(rand.New(rand.NewPCG(seed, 2)), 100, 400)
		again := sweepOrder(rand.New(rand.NewPCG(seed, 2)), 100, 400)
		seen := map[int]bool{}
		for i, index := range order[:100] {
			if index != again[i] {
				t.Fatal("sweep order is not deterministic")
			}
			seen[index] = true
		}
		if len(seen) != 100 {
			t.Fatalf("seed %d: the first 100 steps visit %d distinct keywords, want 100", seed, len(seen))
		}
		if order[100] != order[0] {
			t.Fatal("the sweep does not cycle after one pass")
		}
	}
}

func TestKeywordLikePatternEscapesWildcards(t *testing.T) {
	cases := map[string]string{"违约金": "%违约金%", `5%_a\b`: `%5\%\_a\\b%`}
	for keyword, want := range cases {
		if got := keywordLikePattern(keyword); got != want {
			t.Fatalf("keywordLikePattern(%q) = %q, want %q", keyword, got, want)
		}
	}
}
