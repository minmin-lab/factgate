package main

import (
	"fmt"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
)

// activateKnowledgeProducts proves that every knowledge product the Catalog
// declares has its digest-bound index publication manifest under the snapshot
// artifact directory and that the manifest is the epoch the Catalog binds.
// The gateway refuses to start otherwise, exactly as it does for a missing
// snapshot publication.
func activateKnowledgeProducts(logicalCatalog *catalog.Catalog, artifactDir string) ([]kbpublish.Manifest, error) {
	manifests, err := kbpublish.ActivateFromDirectory(logicalCatalog, artifactDir)
	if err != nil {
		return nil, fmt.Errorf("activate knowledge products: %w", err)
	}
	return manifests, nil
}

// kbEmbedderFor selects the query embedder for the Catalog's knowledge
// products. A learned model needs the trusted embedding service named by
// KB_EMBEDDING_ENDPOINT; without it the Gateway starts, but text queries are
// refused at request time (vector queries still work). The deterministic
// hash-projection provider needs no service. The embedder is checked against
// each product's declared model, dimension and weights before any use.
func kbEmbedderFor(logicalCatalog *catalog.Catalog, endpoint string) (kbpublish.Embedder, string, error) {
	var hashDimension int
	needsService := false
	for _, product := range logicalCatalog.KnowledgeProducts {
		switch product.Embedding.Model {
		case kbpublish.EmbeddingHashProjectionV1:
			if hashDimension != 0 && hashDimension != product.Embedding.Dimension {
				return nil, "", fmt.Errorf("knowledge products declare hash-projection embeddings of different dimensions (%d and %d)", hashDimension, product.Embedding.Dimension)
			}
			hashDimension = product.Embedding.Dimension
		default:
			needsService = true
		}
	}
	switch {
	case needsService && endpoint != "":
		embedder, err := kbpublish.NewHTTPEmbedder(endpoint)
		if err != nil {
			return nil, "", err
		}
		return embedder, "embedding-service", nil
	case needsService:
		return nil, "none (text queries refused)", nil
	case hashDimension != 0:
		return kbpublish.HashEmbedder{Dimension: hashDimension}, kbpublish.EmbeddingHashProjectionV1, nil
	default:
		return nil, "none", nil
	}
}
