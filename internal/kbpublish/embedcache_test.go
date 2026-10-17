package kbpublish

import (
	"errors"
	"testing"
)

type countingEmbedder struct {
	HashEmbedder
	calls, texts int
	fail         bool
}

func (c *countingEmbedder) Embed(texts []string) ([][]float32, error) {
	c.calls++
	c.texts += len(texts)
	if c.fail {
		return nil, errors.New("provider crashed")
	}
	return c.HashEmbedder.Embed(texts)
}

func TestCachingEmbedderResumesAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	inner := &countingEmbedder{HashEmbedder: HashEmbedder{Dimension: 8}}
	cache, err := NewEmbeddingCache(dir, inner.Info())
	if err != nil {
		t.Fatal(err)
	}
	caching := &CachingEmbedder{Inner: inner, Cache: cache}
	first, err := caching.Embed([]string{"a", "b", "c"})
	if err != nil || inner.texts != 3 || caching.Miss != 3 {
		t.Fatalf("first pass: %v texts=%d miss=%d", err, inner.texts, caching.Miss)
	}
	// A "crash": a new embedder over the same directory must not re-encode a, b, c.
	inner2 := &countingEmbedder{HashEmbedder: HashEmbedder{Dimension: 8}}
	cache2, _ := NewEmbeddingCache(dir, inner2.Info())
	resumed := &CachingEmbedder{Inner: inner2, Cache: cache2}
	second, err := resumed.Embed([]string{"a", "b", "c", "d"})
	if err != nil || inner2.texts != 1 || resumed.Hits != 3 || resumed.Miss != 1 {
		t.Fatalf("resume: %v texts=%d hits=%d miss=%d", err, inner2.texts, resumed.Hits, resumed.Miss)
	}
	for index := range first {
		if VectorLiteral(first[index]) != VectorLiteral(second[index]) {
			t.Fatalf("cached vector %d differs", index)
		}
	}
	// A different weights digest is a different cache namespace.
	other, _ := NewEmbeddingCache(dir, EmbedderInfo{Model: "bge-m3", Dimension: 8, WeightsSHA256: "ff"})
	if other.Get("a") != nil {
		t.Fatal("cache leaked across model identities")
	}
	// A provider failure leaves no partial entries behind for the failed batch.
	failing := &CachingEmbedder{Inner: &countingEmbedder{HashEmbedder: HashEmbedder{Dimension: 8}, fail: true}, Cache: cache2}
	if _, err := failing.Embed([]string{"e"}); err == nil || cache2.Get("e") != nil {
		t.Fatal("failed batch must not populate the cache")
	}
}

// noisyEmbedder returns a different vector for every (call, position), the
// way a batched GPU provider's last bits can differ between copies of a text.
type noisyEmbedder struct {
	HashEmbedder
	calls int
	seen  [][]string
}

func (n *noisyEmbedder) Embed(texts []string) ([][]float32, error) {
	n.calls++
	n.seen = append(n.seen, append([]string(nil), texts...))
	vectors, err := n.HashEmbedder.Embed(texts)
	if err != nil {
		return nil, err
	}
	for position := range vectors {
		vectors[position] = append([]float32(nil), vectors[position]...)
		vectors[position][0] += float32(n.calls*1000+position) * 1e-6
	}
	return vectors, nil
}

func TestEmbedDistinctGivesEqualTextsBitIdenticalVectors(t *testing.T) {
	// Positive control: the stub really does return different vectors for
	// two copies of one text, so equality below is the helper's doing.
	control := &noisyEmbedder{HashEmbedder: HashEmbedder{Dimension: 8}}
	raw, err := control.Embed([]string{"a", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if raw[0][0] == raw[1][0] {
		t.Fatal("stub returned identical vectors for two copies; the test would prove nothing")
	}
	noisy := &noisyEmbedder{HashEmbedder: HashEmbedder{Dimension: 8}}
	memo := map[string][]float32{}
	first, err := embedDistinct(noisy, []string{"a", "b", "a"}, memo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := embedDistinct(noisy, []string{"b", "c", "a"}, memo)
	if err != nil {
		t.Fatal(err)
	}
	same := func(x, y []float32) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if !same(first[0], first[2]) || !same(first[0], second[2]) {
		t.Fatal("copies of one text within and across batches carry different vectors")
	}
	if !same(first[1], second[0]) {
		t.Fatal("a text embedded in an earlier batch was embedded again")
	}
	if noisy.calls != 2 || len(noisy.seen[0]) != 2 || len(noisy.seen[1]) != 1 || noisy.seen[1][0] != "c" {
		t.Fatalf("provider saw %v, want each distinct text once", noisy.seen)
	}
}
