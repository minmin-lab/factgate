package kbpublish

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// ManifestPath is where a deployment keeps the kb publication manifest of an
// epoch inside the snapshot artifact directory the gateway already loads.
func ManifestPath(artifactDir, epoch string) string {
	return filepath.Join(artifactDir, "kb", epoch, "kb-publication.json")
}

// LoadManifest reads a kb publication manifest and checks its self-digest.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read kb publication manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode kb publication manifest: %w", err)
	}
	if manifest.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("kb publication manifest version %q is not %s", manifest.Version, ManifestVersion)
	}
	digest, err := manifest.Digest()
	if err != nil {
		return Manifest{}, err
	}
	if digest != manifest.ManifestDigest {
		return Manifest{}, errors.New("kb publication manifest digest does not match its content")
	}
	return manifest, nil
}

// VerifyCatalogBinding checks that a loaded manifest is exactly the epoch the
// Catalog binds for the knowledge product: the declared manifest digest, the
// three publications (name, relation, sidecar, namespace, digests) against the
// Catalog's products and snapshot publications, and the three declaration
// digests. It reads no database; the snapshot loader already proved the
// bundles, and this proves they are the epoch the knowledge product declares.
func VerifyCatalogBinding(cat *catalog.Catalog, product catalog.KnowledgeProduct, manifest Manifest) error {
	binding, ok := cat.LookupKBPublication(product.IndexPublication)
	if !ok || binding.KnowledgeProduct != product.Name {
		return fmt.Errorf("knowledge product %s: Catalog binds no kb publication for epoch %s", product.Name, product.IndexPublication)
	}
	if manifest.ManifestDigest != binding.ManifestDigest {
		return fmt.Errorf("knowledge product %s: manifest digest %s is not the Catalog-bound %s", product.Name, manifest.ManifestDigest, binding.ManifestDigest)
	}
	if manifest.KnowledgeProduct != product.Name || manifest.Epoch != binding.Epoch || manifest.Collection != product.Collection ||
		manifest.SourceNamespace != product.SourceNamespace {
		return fmt.Errorf("knowledge product %s: manifest identity (%s, %s, %s, %s) does not match the Catalog declaration",
			product.Name, manifest.KnowledgeProduct, manifest.Epoch, manifest.Collection, manifest.SourceNamespace)
	}
	if manifest.SegmentationDigest != binding.SegmentationDigest || manifest.EmbeddingDigest != binding.EmbeddingDigest ||
		manifest.ExtractionRulesDigest != binding.ExtractionRulesDigest {
		return fmt.Errorf("knowledge product %s: segmentation, embedding or extraction digests differ from the Catalog binding", product.Name)
	}
	relations := map[string]string{"documents": product.Products.Documents, "chunks": product.Products.Chunks, "values": product.Products.Values}
	if product.Products.CollectionValues != "" {
		relations[CollectionValuesKind] = product.Products.CollectionValues
	}
	if _, published := manifest.Publications[CollectionValuesKind]; published != (product.Products.CollectionValues != "") {
		return fmt.Errorf("knowledge product %s: manifest and Catalog disagree about the %s relation", product.Name, CollectionValuesKind)
	}
	kinds := append([]string(nil), RelationKinds...)
	if product.Products.CollectionValues != "" {
		kinds = append(kinds, CollectionValuesKind)
	}
	for _, kind := range kinds {
		reference, ok := manifest.Publications[kind]
		if !ok {
			return fmt.Errorf("knowledge product %s: manifest lacks the %s publication", product.Name, kind)
		}
		relation, ok := cat.LookupProduct(relations[kind])
		if !ok {
			return fmt.Errorf("knowledge product %s: %s product %q is not in the Catalog", product.Name, kind, relations[kind])
		}
		publication, ok := cat.LookupSnapshotPublication(relation.SnapshotPublication)
		if !ok {
			return fmt.Errorf("knowledge product %s: %s product has no snapshot publication", product.Name, kind)
		}
		if reference.Product != relation.Name || reference.SourceRelation != relation.ReportingView ||
			reference.PublicationName != publication.Name || reference.OrdinalSidecar != publication.OrdinalSidecar ||
			reference.SourceNamespace != publication.SourceNamespace || publication.Snapshot != manifest.Epoch ||
			publication.Source != manifest.CatalogSource {
			return fmt.Errorf("knowledge product %s: %s publication identity differs between manifest and Catalog", product.Name, kind)
		}
		if reference.ManifestDigest != publication.ManifestDigest || reference.DictionaryDigest != publication.DictionaryDigest ||
			reference.SidecarDigest != publication.SidecarDigest {
			return fmt.Errorf("knowledge product %s: %s publication digests differ between manifest and Catalog", product.Name, kind)
		}
	}
	return nil
}

// ActivateFromDirectory verifies every knowledge product the Catalog declares
// against the manifests under artifactDir. Any failure refuses activation.
func ActivateFromDirectory(cat *catalog.Catalog, artifactDir string) ([]Manifest, error) {
	if cat == nil || len(cat.KnowledgeProducts) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(artifactDir) == "" {
		return nil, errors.New("knowledge products are declared but no snapshot artifact directory is configured")
	}
	manifests := make([]Manifest, 0, len(cat.KnowledgeProducts))
	for _, product := range cat.KnowledgeProducts {
		manifest, err := LoadManifest(ManifestPath(artifactDir, product.IndexPublication))
		if err != nil {
			return nil, fmt.Errorf("knowledge product %s: %w", product.Name, err)
		}
		if err := VerifyCatalogBinding(cat, product, manifest); err != nil {
			return nil, err
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}
