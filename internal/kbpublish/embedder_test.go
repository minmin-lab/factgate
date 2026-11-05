package kbpublish

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

func fakeEmbeddingService(t *testing.T, model string, dimension int, weights string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "dimension": dimension, "weights_sha256": weights, "normalized": true})
		case "/embed":
			var request struct{ Texts []string }
			_ = json.NewDecoder(r.Body).Decode(&request)
			vectors := make([][]float32, len(request.Texts))
			for index, text := range request.Texts {
				vector := make([]float32, dimension)
				vector[len(text)%dimension] = 1
				vectors[index] = vector
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "dimension": dimension, "weights_sha256": weights, "vectors": vectors})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHTTPEmbedderBindsModelIdentity(t *testing.T) {
	weights := strings.Repeat("ee", 32)
	server := fakeEmbeddingService(t, EmbeddingBGEM3, 32, weights)
	defer server.Close()
	spec := validSpec()
	spec.Embedding = Embedding{Model: EmbeddingBGEM3, Dimension: 32, WeightsSHA256: weights}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	embedder, err := EmbedderFor(spec, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := embedder.Check(spec.Embedding); err != nil {
		t.Fatalf("matching service refused: %v", err)
	}
	wrong := spec.Embedding
	wrong.WeightsSHA256 = strings.Repeat("00", 32)
	if err := embedder.Check(wrong); err == nil {
		t.Fatal("weights mismatch accepted")
	}
	wrong = spec.Embedding
	wrong.Dimension = 64
	if err := embedder.Check(wrong); err == nil {
		t.Fatal("dimension mismatch accepted")
	}
	_, jsonl, manifest, err := kbcorpus.Build(kbcorpus.Spec{Seed: 5, Documents: 6})
	if err != nil {
		t.Fatal(err)
	}
	spec.Corpus.SHA256 = manifest.DocumentsSHA256
	prepared, err := PrepareWith(spec, jsonl, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Chunks) == 0 || len(prepared.Chunks[0].Embedding) != 32 {
		t.Fatalf("chunks not embedded by the service: %d chunks", len(prepared.Chunks))
	}
	if _, err := EmbedderFor(spec, ""); err == nil {
		t.Fatal("learned model without endpoint accepted")
	}
	// The hash provider refuses a spec that names the learned model.
	if err := (HashEmbedder{Dimension: 32}).Check(spec.Embedding); err == nil {
		t.Fatal("hash provider accepted a bge-m3 spec")
	}
	spec.Embedding = Embedding{Model: EmbeddingHashProjectionV1, Dimension: 32, WeightsSHA256: weights}
	if err := spec.Validate(); err == nil {
		t.Fatal("hash provider with weights accepted")
	}
	spec.Embedding = Embedding{Model: EmbeddingBGEM3, Dimension: 32}
	if err := spec.Validate(); err == nil {
		t.Fatal("bge-m3 without weights accepted")
	}
}
