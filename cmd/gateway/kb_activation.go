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
