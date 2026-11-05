package kbpublish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder turns chunk text into normalized vectors. Info is what the epoch
// binds (model name, dimension, weights digest); Check refuses to publish with
// a provider that is not the one the spec declares.
type Embedder interface {
	Info() EmbedderInfo
	Check(declared Embedding) error
	Embed(texts []string) ([][]float32, error)
}

type EmbedderInfo struct {
	Model         string `json:"model"`
	Dimension     int    `json:"dimension"`
	WeightsSHA256 string `json:"weights_sha256,omitempty"`
}

func checkDeclared(info EmbedderInfo, declared Embedding) error {
	if info.Model != declared.Model || info.Dimension != declared.Dimension || info.WeightsSHA256 != declared.WeightsSHA256 {
		return fmt.Errorf("embedding provider reports %s/%d/%s but the spec declares %s/%d/%s",
			info.Model, info.Dimension, info.WeightsSHA256, declared.Model, declared.Dimension, declared.WeightsSHA256)
	}
	return nil
}

// HashEmbedder is hash-projection-v1 in-process.
type HashEmbedder struct{ Dimension int }

func (h HashEmbedder) Info() EmbedderInfo {
	return EmbedderInfo{Model: EmbeddingHashProjectionV1, Dimension: h.Dimension}
}
func (h HashEmbedder) Check(declared Embedding) error { return checkDeclared(h.Info(), declared) }
func (h HashEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for index, text := range texts {
		out[index] = Embed(text, h.Dimension)
	}
	return out, nil
}

// HTTPEmbedder talks to the trusted embedding service (~/kb-embed/service.py):
// GET /model for identity, POST /embed {"texts": [...]} for vectors.
type HTTPEmbedder struct {
	Endpoint string
	Client   *http.Client
	info     *EmbedderInfo
}

func NewHTTPEmbedder(endpoint string) (*HTTPEmbedder, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("embedding endpoint is required for a learned model")
	}
	embedder := &HTTPEmbedder{Endpoint: endpoint, Client: &http.Client{Timeout: 10 * time.Minute}}
	response, err := embedder.Client.Get(endpoint + "/model")
	if err != nil {
		return nil, fmt.Errorf("embedding service /model: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service /model returned %d", response.StatusCode)
	}
	var info struct {
		Model         string `json:"model"`
		Dimension     int    `json:"dimension"`
		WeightsSHA256 string `json:"weights_sha256"`
		Normalized    bool   `json:"normalized"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode embedding service /model: %w", err)
	}
	if !info.Normalized {
		return nil, errors.New("embedding service does not normalize vectors; cosine distance would be undefined")
	}
	embedder.info = &EmbedderInfo{Model: info.Model, Dimension: info.Dimension, WeightsSHA256: info.WeightsSHA256}
	return embedder, nil
}

func (h *HTTPEmbedder) Info() EmbedderInfo             { return *h.info }
func (h *HTTPEmbedder) Check(declared Embedding) error { return checkDeclared(h.Info(), declared) }

func (h *HTTPEmbedder) Embed(texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"texts": texts})
	if err != nil {
		return nil, err
	}
	response, err := h.Client.Post(h.Endpoint+"/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding service /embed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service /embed returned %d", response.StatusCode)
	}
	var payload struct {
		Model         string      `json:"model"`
		Dimension     int         `json:"dimension"`
		WeightsSHA256 string      `json:"weights_sha256"`
		Vectors       [][]float32 `json:"vectors"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<30)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode embedding service /embed: %w", err)
	}
	if payload.Model != h.info.Model || payload.WeightsSHA256 != h.info.WeightsSHA256 || payload.Dimension != h.info.Dimension {
		return nil, errors.New("embedding service identity changed between /model and /embed")
	}
	return payload.Vectors, nil
}

// EmbedderFor selects the provider the spec names. The endpoint is only used
// for learned models.
func EmbedderFor(spec Spec, endpoint string) (Embedder, error) {
	switch spec.Embedding.Model {
	case EmbeddingHashProjectionV1:
		return HashEmbedder{Dimension: spec.Embedding.Dimension}, nil
	case EmbeddingBGEM3:
		return NewHTTPEmbedder(endpoint)
	default:
		return nil, fmt.Errorf("unknown embedding model %q", spec.Embedding.Model)
	}
}
