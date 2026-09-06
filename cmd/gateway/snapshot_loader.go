package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	maxSnapshotBundleManifestBytes = 4 << 20
	// Cumulative guard on the HOT dictionaries this Gateway process retains for
	// the whole deployed Catalog. COLD envelopes and sidecar semantics are
	// streamed through bounded verifiers and are never retained in the heap.
	//
	// This is not the per-profile-closure limit (finalv5profile.MaxHotBytesPerInstance,
	// 1 GiB, the "per Catalog-bound Gateway instance HOT limit" the paper cites):
	// every production profile activates only its own closure, which stays under
	// that limit. This constant bounds the one deployment that activates the full
	// live Catalog at once -- the integration compose-e2e wiring gate -- whose
	// eight publications' HOT dictionaries sum to ~1.21 GiB once the P9.E
	// scale-e7 publication (907 MiB alone) joined the Catalog in v1.13. Sized at
	// 2 GiB to match the sibling combined-artifact envelope (v4-offline
	// maxPublishedBytes) with headroom, and far under the Gateway's 12 GiB cgroup
	// ceiling alongside its measured ~7.35 GiB accounting peak.
	maxGatewayHotArtifactsBytes = 2 << 30
	// Streaming COLD verification must not turn the complete audit artifact
	// into charged cgroup page cache.  Drop already-consumed pages in bounded
	// windows; the verifier retains only its fixed-size userspace buffer.
	snapshotCacheDropWindowBytes = 8 << 20
)

type snapshotPublicationStore interface {
	EnforceExposureDeploymentMode(context.Context, string, bool) error
	PutOrdinalSnapshotPublication(context.Context, string, ordinal.SnapshotIndex, []control.OrdinalArtifactChunk) error
}

type loadedSnapshotPublication struct {
	publication catalog.SnapshotPublication
	index       *ordinal.HotDictionary
	hotBytes    int64
}

func snapshotRegistryFromEnv(ctx context.Context, logicalCatalog *catalog.Catalog, store snapshotPublicationStore) (*ordinal.Registry, error) {
	directory := strings.TrimSpace(os.Getenv("GATEWAY_SNAPSHOT_ARTIFACT_DIR"))
	if directory == "" {
		return nil, nil
	}
	return loadSnapshotArtifactDirectory(ctx, directory, logicalCatalog, store)
}

// activatedSnapshotState is captured by the loader for the read-only profile
// diagnostic. It is written exactly once, during startup verification.
var activatedSnapshotState *activationState

// loadSnapshotArtifactDirectory activates exactly the publications declared by
// the already validated Catalog. Unknown, missing, symlinked, oversized, or
// digest-mismatched artifacts abort startup before a registry is returned.
func loadSnapshotArtifactDirectory(ctx context.Context, directory string, logicalCatalog *catalog.Catalog,
	store snapshotPublicationStore) (*ordinal.Registry, error) {
	if logicalCatalog == nil || store == nil || strings.TrimSpace(directory) == "" || logicalCatalog.SHA256 == "" {
		return nil, errors.New("snapshot artifact loader requires Catalog, Control store, and directory")
	}
	baseInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("stat snapshot artifact directory: %w", err)
	}
	if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("snapshot artifact path must be a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read snapshot artifact directory: %w", err)
	}
	expected := make(map[string]catalog.SnapshotPublication, len(logicalCatalog.SnapshotPublications))
	for _, publication := range logicalCatalog.SnapshotPublications {
		expected[publication.Name] = publication
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}
		if !entry.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unexpected non-directory snapshot artifact entry %q", entry.Name())
		}
		if _, found := expected[entry.Name()]; !found {
			return nil, fmt.Errorf("snapshot artifact directory contains undeclared publication %q", entry.Name())
		}
		if _, duplicate := seen[entry.Name()]; duplicate {
			return nil, fmt.Errorf("duplicate snapshot publication directory %q", entry.Name())
		}
		seen[entry.Name()] = struct{}{}
	}
	if len(seen) != len(expected) {
		for _, publication := range logicalCatalog.SnapshotPublications {
			if _, found := seen[publication.Name]; !found {
				return nil, fmt.Errorf("snapshot publication %q has no artifact directory", publication.Name)
			}
		}
	}

	loaded := make([]loadedSnapshotPublication, 0, len(logicalCatalog.SnapshotPublications))
	var totalHotBytes int64
	for _, publication := range logicalCatalog.SnapshotPublications {
		value, hotBytes, loadErr := loadSnapshotPublication(directory, logicalCatalog, publication,
			maxGatewayHotArtifactsBytes-totalHotBytes)
		if loadErr != nil {
			return nil, fmt.Errorf("load snapshot publication %q: %w", publication.Name, loadErr)
		}
		totalHotBytes += hotBytes
		value.hotBytes = hotBytes
		loaded = append(loaded, value)
	}
	// Persist the one-way deployment mode only after every Catalog publication
	// has been verified, but before any registry/Control metadata is published.
	// The Control transaction serializes this activation with legacy writes.
	if len(logicalCatalog.SnapshotPublications) != 0 {
		if err := store.EnforceExposureDeploymentMode(ctx, logicalCatalog.SHA256, true); err != nil {
			return nil, fmt.Errorf("V4 clean cutover activation: %w", err)
		}
	}

	registry, err := ordinal.NewRegistry()
	if err != nil {
		return nil, err
	}
	for _, value := range loaded {
		if err := registry.RegisterPublication(ordinal.PublicationKey{CatalogDigest: logicalCatalog.SHA256,
			PublicationName: value.publication.Name}, value.publication.ManifestDigest, value.index); err != nil {
			return nil, fmt.Errorf("register snapshot publication %q: %w", value.publication.Name, err)
		}
	}
	for _, value := range loaded {
		// The publication manifest digest is the immutable Catalog-approved
		// publication identity. Artifacts remain in the external deployment:
		// putting nil chunks stores only verified dictionary/segment metadata.
		if err := store.PutOrdinalSnapshotPublication(ctx, value.publication.ManifestDigest, value.index, nil); err != nil {
			return nil, fmt.Errorf("persist snapshot publication %q metadata: %w", value.publication.Name, err)
		}
	}
	// Record what this process actually parsed, so the activation diagnostic
	// reports observed state instead of echoing a Catalog back to its caller.
	observed := make([]activatedHotArtifact, 0, len(loaded))
	for _, value := range loaded {
		observed = append(observed, activatedHotArtifact{Publication: value.publication.Name,
			ManifestDigest: value.publication.ManifestDigest,
			HotIndexDigest: value.index.Manifest().HotIndexDigest, Bytes: value.hotBytes})
	}
	activatedSnapshotState = newActivationState(logicalCatalog, observed, totalHotBytes, maxGatewayHotArtifactsBytes)
	// The Catalog-wide dictionary universe is persisted lazily at query
	// compilation/settlement. Startup only proves every member publication; it
	// does not create query/control state before an authorized request exists.
	return registry, nil
}

func loadSnapshotPublication(baseDirectory string, logicalCatalog *catalog.Catalog, publication catalog.SnapshotPublication,
	remainingHotBytes int64) (loadedSnapshotPublication, int64, error) {
	directory := filepath.Join(baseDirectory, publication.Name)
	if err := verifyPublicationDirectoryShape(directory, publication.Name); err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	manifestPath := filepath.Join(directory, publication.Name+".bundle.json")
	manifestBytes, err := readVerifiedRegularFile(manifestPath, maxSnapshotBundleManifestBytes)
	if err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	bundleManifest, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	if err := matchBundleToCatalog(logicalCatalog, publication, bundleManifest); err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	if bundleManifest.Hot.Bytes > remainingHotBytes || remainingHotBytes < 0 {
		return loadedSnapshotPublication{}, 0, fmt.Errorf(
			"Catalog hot artifacts exceed the %d MiB activation boundary", maxGatewayHotArtifactsBytes>>20)
	}
	hotPath := filepath.Join(directory, bundleManifest.Hot.Name)
	hotBytes, err := readVerifiedRegularFile(hotPath, remainingHotBytes)
	if err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	if err := verifyFileDescriptor(bundleManifest.Hot, hotBytes); err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	hot, err := ordinal.ParseHotDictionary(hotBytes, publication.ManifestDigest)
	if err != nil {
		return loadedSnapshotPublication{}, 0, fmt.Errorf("parse manifest-bound HOT artifact: %w", err)
	}
	if err := matchHotIndexToCatalog(logicalCatalog, publication, bundleManifest, hot); err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	// Separating the attestation domains removed the only implicit tie between
	// a Product and the bundle it reads through, so the binding is checked here
	// explicitly. See docs/final_v5_c15_attestation_separation.md.
	if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err != nil {
		return loadedSnapshotPublication{}, 0, err
	}
	hotArtifactBytes := int64(len(hotBytes))
	// MarshalBinaryWithoutEntityKeys is valid for a connector that has a
	// separately attested sidecar, but this startup path promises to verify the
	// Catalog sidecar digest itself. Require the entity-key-bearing HOT variant;
	// ParseHotDictionary has already checked every row against SidecarDigest.
	first, found := hot.LookupRow(1)
	firstHandle, reverseFound := hot.LookupRowHandle(first.EntityKey)
	if !found || first.EntityKey == "" || !reverseFound || firstHandle != 1 {
		return loadedSnapshotPublication{}, 0, errors.New("HOT artifact omits the manifest-bound entity sidecar")
	}
	// Parsing owns independent hash/row arrays. Release the transport buffer
	// before streaming the much larger COLD/sidecar artifacts and before the
	// next publication is loaded, otherwise multiple raw HOT files can overlap
	// their resident decoded indexes at startup. This changes no verified bytes
	// or index lifetime; it only enforces the intended HOT working-set bound.
	hotBytes = nil
	debug.FreeOSMemory()
	// The compiler already verified every COLD FactID and semantic root before
	// atomically publishing this directory. Startup preserves the warm HOT-index
	// activation SLO: on the production read-only artifact mount it streams only
	// the COLD envelope (Catalog-bound embedded manifest, full-file seal/SHA) and
	// does not redo the compiler's million-Fact JSON/canonicalization pass.
	// Sidecars are small and are fully replayed against the verified HOT index.
	if err := verifyColdArtifact(filepath.Join(directory, bundleManifest.Cold.Name), bundleManifest.Cold,
		publication.ManifestDigest); err != nil {
		return loadedSnapshotPublication{}, 0, fmt.Errorf("COLD artifact verification: %w", err)
	}
	sidecarExpectation := snapshotbundle.SidecarExpectation{PublicationName: publication.Name,
		OrdinalSidecar: publication.OrdinalSidecar, SourceNamespace: publication.SourceNamespace,
		ManifestDigest: publication.ManifestDigest, SidecarDigest: publication.SidecarDigest}
	if err := verifySidecarArtifact(filepath.Join(directory, bundleManifest.Sidecar.Name), bundleManifest.Sidecar,
		hot, sidecarExpectation); err != nil {
		return loadedSnapshotPublication{}, 0, fmt.Errorf("sidecar export verification: %w", err)
	}
	return loadedSnapshotPublication{publication: publication, index: hot}, hotArtifactBytes, nil
}

func verifyPublicationDirectoryShape(directory, publicationName string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication artifact path must be a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	expected := map[string]struct{}{
		publicationName + ".bundle.json": {}, publicationName + ".hot.tgord": {},
		publicationName + ".cold.tgord": {}, publicationName + ".sidecar.ndjson": {},
	}
	if len(entries) != len(expected) {
		return errors.New("publication artifact directory is incomplete or contains unknown files")
	}
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found {
			return fmt.Errorf("unexpected publication artifact %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication artifact %q is not a regular file", entry.Name())
		}
	}
	return nil
}

func matchBundleToCatalog(logicalCatalog *catalog.Catalog, publication catalog.SnapshotPublication,
	bundle snapshotbundle.BundleManifest) error {
	if bundle.PublicationName != publication.Name || bundle.CatalogSource != publication.Source ||
		bundle.OrdinalSidecar != publication.OrdinalSidecar || bundle.ManifestDigest != publication.ManifestDigest ||
		bundle.DictionaryManifest.DictionaryDigest != publication.DictionaryDigest ||
		bundle.DictionaryManifest.SidecarDigest != publication.SidecarDigest ||
		bundle.DictionaryManifest.SourceNamespace != publication.SourceNamespace ||
		bundle.DictionaryManifest.Snapshot != publication.Snapshot {
		return errors.New("bundle does not match the Catalog snapshot publication")
	}
	source, found := catalogSource(logicalCatalog, publication.Source)
	if !found || bundle.DictionaryManifest.SourceID != source.DatasourceID {
		return errors.New("bundle does not match the Catalog datasource")
	}
	// The bundle's embedded schema attestation is deliberately not compared
	// with Source.SchemaDigest. They answer different questions: the bundle
	// records the attestation this immutable Publication was compiled under,
	// while Source.SchemaDigest attests the reporting surface the *active
	// profile* declares. One Publication is shared by several profiles, so no
	// single embedded value could equal all of their surface attestations.
	//
	// The build attestation stays fully bound: DictionaryManifest.Digest folds
	// SchemaDigest in, and the ManifestDigest equality above is checked against
	// the Catalog-approved publication identity, so editing it still fails
	// closed as the Publication identity violation it is. See
	// docs/final_v5_c15_attestation_separation.md.
	return nil
}

func matchHotIndexToCatalog(logicalCatalog *catalog.Catalog, publication catalog.SnapshotPublication,
	bundle snapshotbundle.BundleManifest, hot *ordinal.HotDictionary) error {
	manifest := hot.Manifest()
	digest, err := manifest.Digest()
	if err != nil || digest != publication.ManifestDigest || hot.ManifestDigest() != publication.ManifestDigest ||
		hot.DictionaryDigest() != publication.DictionaryDigest || manifest.SidecarDigest != publication.SidecarDigest ||
		manifest.SourceNamespace != publication.SourceNamespace || manifest.Snapshot != publication.Snapshot ||
		bundle.RowCount != hot.RowCount() || !reflect.DeepEqual(manifest, bundle.DictionaryManifest) {
		return errors.New("HOT index does not match Catalog and bundle manifests")
	}
	source, found := catalogSource(logicalCatalog, publication.Source)
	if !found || manifest.SourceID != source.DatasourceID {
		return errors.New("HOT index datasource binding mismatch")
	}
	// Same separation as matchBundleToCatalog. The HOT manifest is already
	// required to be DeepEqual to the bundle manifest and to recompute to the
	// Catalog-approved ManifestDigest, which covers its schema attestation.
	return nil
}

func catalogSource(logicalCatalog *catalog.Catalog, name string) (catalog.Source, bool) {
	for _, source := range logicalCatalog.Sources {
		if source.Name == name {
			return source, true
		}
	}
	return catalog.Source{}, false
}

func readVerifiedRegularFile(path string, maximum int64) ([]byte, error) {
	file, size, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if size <= 0 || size > maximum || size > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("artifact %q has invalid or excessive size", filepath.Base(path))
	}
	payload, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(payload)) != size {
		return nil, fmt.Errorf("read artifact %q: size changed", filepath.Base(path))
	}
	// The returned userspace copy is the authoritative input.  Retaining the
	// same bytes in the cgroup page cache only double-counts activation memory.
	dropFileCache(file, 0, size)
	return payload, nil
}

func verifyColdArtifact(path string, descriptor snapshotbundle.FileDescriptor, expectedManifestDigest string) error {
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("COLD artifact size differs from its bundle descriptor")
	}
	reader := newCacheDroppingReader(file)
	digest, err := ordinal.VerifyColdDictionaryEnvelopeReader(reader, size, expectedManifestDigest)
	reader.dropConsumed()
	if err != nil {
		return err
	}
	if descriptor.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("COLD artifact SHA-256 differs from its bundle descriptor")
	}
	return nil
}

func verifySidecarArtifact(path string, descriptor snapshotbundle.FileDescriptor, index ordinal.SnapshotIndex,
	expected snapshotbundle.SidecarExpectation) error {
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("sidecar artifact size differs from its bundle descriptor")
	}
	hash := sha256.New()
	reader := newCacheDroppingReader(file)
	// Verification can fail after consuming an arbitrary prefix. Drop that
	// prefix on every return path so a corrupt deployment cannot leave its
	// scanned sidecar charged to the Gateway cgroup until process exit.
	defer reader.dropConsumed()
	if err := snapshotbundle.VerifySidecarNDJSON(io.TeeReader(reader, hash), index, expected); err != nil {
		return err
	}
	if descriptor.SHA256 != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("sidecar artifact SHA-256 differs from its bundle descriptor")
	}
	return nil
}

// cacheDroppingReader bounds file-backed cgroup memory while preserving the
// exact streaming verifier. FADV_DONTNEED is only a cache hint: correctness
// continues to come from the hashes and canonical parsers above.
type cacheDroppingReader struct {
	file       *os.File
	offset     int64
	dropOffset int64
}

func newCacheDroppingReader(file *os.File) *cacheDroppingReader {
	if file != nil {
		adviseSequentialFile(file)
	}
	return &cacheDroppingReader{file: file}
}

func (r *cacheDroppingReader) Read(payload []byte) (int, error) {
	if r == nil || r.file == nil {
		return 0, io.EOF
	}
	count, err := r.file.Read(payload)
	r.offset += int64(count)
	if r.offset-r.dropOffset >= snapshotCacheDropWindowBytes {
		r.dropConsumed()
	}
	return count, err
}

func (r *cacheDroppingReader) dropConsumed() {
	if r == nil || r.file == nil || r.offset <= r.dropOffset {
		return
	}
	dropFileCache(r.file, r.dropOffset, r.offset-r.dropOffset)
	r.dropOffset = r.offset
}

func openRegularFile(path string) (*os.File, int64, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("artifact %q is not a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, fmt.Errorf("artifact %q changed while opening", filepath.Base(path))
	}
	return file, after.Size(), nil
}

func verifyFileDescriptor(descriptor snapshotbundle.FileDescriptor, payload []byte) error {
	digest := sha256.Sum256(payload)
	if descriptor.Bytes != int64(len(payload)) || descriptor.SHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("artifact %q digest/size differs from bundle", descriptor.Name)
	}
	return nil
}
