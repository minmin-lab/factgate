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
