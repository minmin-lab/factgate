package kbpublish

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// EmbeddingCache persists one vector per (weights, text) on disk as soon as it
// is computed, so an encode interrupted by a crash resumes where it stopped.
// Layout: <dir>/<model>-<weights_sha256 or 'none'>/<ab>/<text_sha256>.f32le,
// little-endian float32, dimension implied by file size. Writes go through a
// temporary file and rename, so a half-written vector is never read back.
type EmbeddingCache struct {
	root      string
	dimension int
}

func NewEmbeddingCache(dir string, info EmbedderInfo) (*EmbeddingCache, error) {
	if dir == "" {
		return nil, errors.New("embedding cache directory is required")
	}
	weights := info.WeightsSHA256
	if weights == "" {
		weights = "none"
	}
	root := filepath.Join(dir, info.Model+"-"+weights)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &EmbeddingCache{root: root, dimension: info.Dimension}, nil
}

func (c *EmbeddingCache) path(text string) string {
	sum := sha256.Sum256([]byte(text))
	key := hex.EncodeToString(sum[:])
	return filepath.Join(c.root, key[:2], key+".f32le")
}

// Get returns the cached vector for text, or nil.
func (c *EmbeddingCache) Get(text string) []float32 {
	data, err := os.ReadFile(c.path(text))
	if err != nil || len(data) != 4*c.dimension {
		return nil
	}
	vector := make([]float32, c.dimension)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*index:]))
	}
	return vector
}

// Put stores a vector atomically.
func (c *EmbeddingCache) Put(text string, vector []float32) error {
	if len(vector) != c.dimension {
		return fmt.Errorf("cache dimension %d, vector %d", c.dimension, len(vector))
	}
	target := c.path(text)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	data := make([]byte, 4*len(vector))
	for index, component := range vector {
		binary.LittleEndian.PutUint32(data[4*index:], math.Float32bits(component))
	}
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

// CachingEmbedder wraps an Embedder with an EmbeddingCache: cached texts are
// never sent to the provider, new vectors are persisted before Embed returns.
type CachingEmbedder struct {
	Inner Embedder
	Cache *EmbeddingCache
	Hits  int
	Miss  int
}

func (e *CachingEmbedder) Info() EmbedderInfo             { return e.Inner.Info() }
func (e *CachingEmbedder) Check(declared Embedding) error { return e.Inner.Check(declared) }

func (e *CachingEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var pending []string
	var positions []int
	for index, text := range texts {
		if vector := e.Cache.Get(text); vector != nil {
			out[index] = vector
			e.Hits++
			continue
		}
		pending = append(pending, text)
		positions = append(positions, index)
	}
	if len(pending) > 0 {
		vectors, err := e.Inner.Embed(pending)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(pending) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(pending))
		}
		for offset, vector := range vectors {
			if err := e.Cache.Put(pending[offset], vector); err != nil {
				return nil, fmt.Errorf("persist embedding: %w", err)
			}
			out[positions[offset]] = vector
			e.Miss++
		}
	}
	return out, nil
}
