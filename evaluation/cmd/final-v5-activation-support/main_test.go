package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func committedRegistry(t *testing.T) finalv5profile.Registry {
	t.Helper()
	registry, err := loadRegistry(filepath.Join(repositoryRoot(t), registryPath))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func committedSupport(t *testing.T) finalv5profile.ActivationSupport {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), supportPath))
	if os.IsNotExist(err) {
		// Activation support does not carry across a contract release, so a
		// freshly amended release legitimately has no manifest until its
		// profiles are re-activated. The registry must then claim no support,
		// which TestRegistryClaimsNoSupportWithoutAManifest checks.
		t.Skip("no activation support manifest for this contract release yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	support, err := finalv5profile.DecodeActivationSupport(payload)
	if err != nil {
		t.Fatalf("committed activation support manifest is invalid: %v", err)
	}
	return support
}

// The proven live-route profiles (the sixteen prior profiles plus the four
// refused-footprint ladder arms, the three benign-trace arms, the four
// comparator arms, and the scale7 SUM-ladder arm, each proven by its live
// fixed point) have completed a smoke under the current contract release.
// Evidence from an earlier release does not carry forward.
func TestCommittedManifestSupportsExactlyTheCurrentReleaseProvenProfiles(t *testing.T) {
	support := committedSupport(t)
	proven := map[string]bool{
		"rls-unlimited": true, "expense-detail": true, "attack-expense-detail": true,
		"rls-bounded": true, "concurrency-expense-detail": true,
		"provsql-nonce-join": true, "result-heavy": true, "exposure-scale": true,
		"depth4-semantic-view": true, "analytics-orders-lineitem": true,
		"analytics-orders": true, "footprint-bounded": true, "footprint-unlimited": true,
		"benign-recipe": true, "benign-x2": true, "benign-x4": true,
		"counter-exact": true, "counter-rows": true, "counter-queries": true, "counter-release": true,
		"adversary-owner": true, "adversary-tightened": true, "adversary-loosened": true,
		"scale7": true,
	}

	seen := map[string]bool{}
	for _, profile := range support.Profiles {
		seen[profile.ProfileAlias] = true
		switch {
		case proven[profile.ProfileAlias]:
			if !profile.ActivationSupported || !profile.ActivationSmokePassed {
				t.Errorf("proven profile %s is not supported", profile.ProfileAlias)
			}
			if len(profile.ActivationEvidenceSHA256) == 0 {
				t.Errorf("proven profile %s carries no evidence digest", profile.ProfileAlias)
			}
		default:
			t.Errorf("unexpected profile %s in the manifest", profile.ProfileAlias)
		}
	}
	for alias := range proven {
		if !seen[alias] {
			t.Errorf("proven profile %s is missing from the manifest", alias)
		}
	}
}

// Result-heavy's current-release claim is backed by the one live activation
// smoke that established this release's targeted-run prerequisite.
func TestResultHeavyCarriesTheCurrentReleaseActivationEvidence(t *testing.T) {
	for _, profile := range committedSupport(t).Profiles {
		if profile.ProfileAlias != "result-heavy" {
			continue
		}
		if len(profile.ActivationEvidenceSHA256) != 1 {
			t.Fatalf("result-heavy carries %d evidence digests, want 1 current-release activation",
				len(profile.ActivationEvidenceSHA256))
		}
		return
	}
	t.Fatal("result-heavy is absent from the activation support manifest")
}

// Without a manifest for this contract release, no profile may claim support.
// This is what stops a re-freeze from silently inheriting the previous
// release's activation evidence.
func TestRegistryClaimsNoSupportWithoutAManifest(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repositoryRoot(t), supportPath)); !os.IsNotExist(err) {
		t.Skip("this contract release has an activation support manifest")
	}
	for _, profile := range committedRegistry(t).Profiles {
		if profileClaimsActivationSupport(profile) {
			t.Errorf("%s claims activation support with no manifest for this release", profile.Alias)
		}
	}
}

func writeRegistryFixture(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	profile := map[string]any{
		"profile_id": "profile-test", "alias": "test",
		"closure":        map[string]any{"closure_sha256": strings.Repeat("b", 64)},
		"status":         map[string]any{},
		"catalog_sha256": strings.Repeat("a", 64),
	}
	if mutate != nil {
		mutate(profile)
	}
	writeFixtureDocument(t, root, registryPath, map[string]any{
		"schema_version": 1, "registry_version": finalv5profile.RegistryVersion,
		"contract_release": "final-v5-contracts-v1.4", "profiles": []any{profile},
	})
}

// The no-manifest invariant is exercised independently of the repository's
// current release state. The committed-state test above becomes runnable again
// at P4; this fixture keeps every form of the invariant covered now.
func TestNoManifestRejectsEveryRegistryActivationClaim(t *testing.T) {
	t.Run("no support claim", func(t *testing.T) {
		root := t.TempDir()
		writeRegistryFixture(t, root, nil)
		if err := run(root, "", true); err != nil {
			t.Fatalf("an unsupported registry was rejected without a manifest: %v", err)
		}
	})

	mutations := map[string]func(map[string]any){
		"activation supported": func(profile map[string]any) {
			profile["status"].(map[string]any)["activation_supported"] = true
		},
		"activation smoke passed": func(profile map[string]any) {
			profile["status"].(map[string]any)["activation_smoke_passed"] = true
		},
		"targeted run eligible": func(profile map[string]any) {
			profile["targeted_run_eligible"] = true
		},
		"routable": func(profile map[string]any) {
			profile["routable"] = true
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeRegistryFixture(t, root, mutate)
			err := run(root, "", true)
			if err == nil || !strings.Contains(err.Error(),
				"profile test claims activation support with no manifest") {
				t.Fatalf("activation claim without a manifest error = %v", err)
			}
		})
	}
}

func TestDeploymentEvidenceDoesNotCarryAcrossAContractRelease(t *testing.T) {
	const current = "final-v5-contracts-v1.4"
	for name, document := range map[string]map[string]any{
		"missing release":  {},
		"previous release": {"contract_release": "final-v5-contracts-v1.3"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := requireEvidenceContractRelease("route matrix", document, current); err == nil {
				t.Fatal("deployment-wide execution evidence crossed a contract-release boundary")
			}
		})
	}
	if err := requireEvidenceContractRelease("route matrix",
		map[string]any{"contract_release": current}, current); err != nil {
		t.Fatalf("current-release deployment evidence was rejected: %v", err)
	}
}

type deploymentEvidenceFixture struct {
	root                           string
	registry, intersection, matrix map[string]any
	isolation, production          map[string]any
	registryDigest                 string
	intersectionDigest             string
	matrixDigest                   string
	isolationDigest                string
	productionDigest               string
}

func newDeploymentEvidenceFixture(t *testing.T, contractRelease string) *deploymentEvidenceFixture {
	t.Helper()
	fixture := &deploymentEvidenceFixture{root: t.TempDir()}
	fixture.registry = map[string]any{
		"schema_version": 1, "contract_release": contractRelease,
	}
	fixture.registryDigest = writeFixtureDocument(t, fixture.root, registryPath,
		fixture.registry)
	fixture.intersection = map[string]any{
		"record":            "taskgate-final-v5-product-intersection-v1",
		"contracts_version": contractRelease,
	}
	fixture.intersectionDigest = writeFixtureDocument(t, fixture.root, intersectionPath,
		fixture.intersection)
	fixture.matrix = map[string]any{
		"record":                             "taskgate-final-v5-outside-product-route-matrix-v1",
		"contract_release":                   contractRelease,
		"profile_registry_sha256":            fixture.registryDigest,
		"product_intersection_matrix_sha256": fixture.intersectionDigest,
		"status":                             "pass",
		"failed_probe_count":                 0,
	}
	fixture.matrixDigest = writeFixtureDocument(t, fixture.root, routeMatrixPath, fixture.matrix)
	fixture.production = map[string]any{
		"record": "taskgate-final-v5-production-semantic-cache-lookup-v1",
	}
	fixture.productionDigest = writeFixtureDocument(t, fixture.root, productionLookupPath,
		fixture.production)
	fixture.isolation = map[string]any{
		"record":                              "taskgate-final-v5-semantic-cache-isolation-evidence-v1",
		"contract_release":                    contractRelease,
		"profile_registry_sha256":             fixture.registryDigest,
		"product_intersection_matrix_sha256":  fixture.intersectionDigest,
		"outside_product_route_matrix_sha256": fixture.matrixDigest,
		"production_lookup_manifest_sha256":   fixture.productionDigest,
		"status":                              "pass",
		"semantic_cache_catalog_bound":        true,
	}
	fixture.isolationDigest = writeFixtureDocument(t, fixture.root, isolationPath,
		fixture.isolation)
	return fixture
}

func writeFixtureDocument(t *testing.T, root, relativePath string, document map[string]any) string {
	t.Helper()
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestLoadDeploymentEvidenceRequiresAConsistentCurrentReleaseChain(t *testing.T) {
	const current = "final-v5-contracts-v1.4"
	if _, err := loadDeploymentEvidence(newDeploymentEvidenceFixture(t, current).root, current); err != nil {
		t.Fatalf("valid current-release deployment evidence was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*deploymentEvidenceFixture){
		"intersection missing release": func(f *deploymentEvidenceFixture) {
			delete(f.intersection, "contracts_version")
			writeFixtureDocument(t, f.root, intersectionPath, f.intersection)
		},
		"intersection stale release": func(f *deploymentEvidenceFixture) {
			f.intersection["contracts_version"] = "final-v5-contracts-v1.3"
			writeFixtureDocument(t, f.root, intersectionPath, f.intersection)
		},
		"route matrix missing release": func(f *deploymentEvidenceFixture) {
			delete(f.matrix, "contract_release")
			writeFixtureDocument(t, f.root, routeMatrixPath, f.matrix)
		},
		"route matrix stale release": func(f *deploymentEvidenceFixture) {
			f.matrix["contract_release"] = "final-v5-contracts-v1.3"
			writeFixtureDocument(t, f.root, routeMatrixPath, f.matrix)
		},
		"isolation missing release": func(f *deploymentEvidenceFixture) {
			delete(f.isolation, "contract_release")
			writeFixtureDocument(t, f.root, isolationPath, f.isolation)
		},
		"isolation stale release": func(f *deploymentEvidenceFixture) {
			f.isolation["contract_release"] = "final-v5-contracts-v1.3"
			writeFixtureDocument(t, f.root, isolationPath, f.isolation)
		},
		"isolation names another route matrix": func(f *deploymentEvidenceFixture) {
			f.isolation["outside_product_route_matrix_sha256"] = strings.Repeat("b", 64)
			writeFixtureDocument(t, f.root, isolationPath, f.isolation)
		},
		"route matrix names another intersection matrix": func(f *deploymentEvidenceFixture) {
			f.matrix["product_intersection_matrix_sha256"] = strings.Repeat("b", 64)
			writeFixtureDocument(t, f.root, routeMatrixPath, f.matrix)
		},
		"isolation names another profile registry": func(f *deploymentEvidenceFixture) {
			f.isolation["profile_registry_sha256"] = strings.Repeat("b", 64)
			writeFixtureDocument(t, f.root, isolationPath, f.isolation)
		},
		"route matrix names another profile registry": func(f *deploymentEvidenceFixture) {
			f.matrix["profile_registry_sha256"] = strings.Repeat("b", 64)
			writeFixtureDocument(t, f.root, routeMatrixPath, f.matrix)
		},
		"isolation names another production lookup manifest": func(f *deploymentEvidenceFixture) {
			f.isolation["production_lookup_manifest_sha256"] = strings.Repeat("b", 64)
			writeFixtureDocument(t, f.root, isolationPath, f.isolation)
		},
		"profile registry bytes changed": func(f *deploymentEvidenceFixture) {
			f.registry["changed"] = true
			writeFixtureDocument(t, f.root, registryPath, f.registry)
		},
		"production lookup manifest bytes changed": func(f *deploymentEvidenceFixture) {
			f.production["changed"] = true
			writeFixtureDocument(t, f.root, productionLookupPath, f.production)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newDeploymentEvidenceFixture(t, current)
			mutate(fixture)
			if _, err := loadDeploymentEvidence(fixture.root, current); err == nil {
				t.Fatal("inconsistent deployment-wide evidence was accepted")
			}
		})
	}
}

func TestV13GoldenPassesTheActivationSupportDigestConsumer(t *testing.T) {
	root := t.TempDir()
	fixtures := map[string]string{
		"registry": registryPath, "intersection": intersectionPath,
		"route": routeMatrixPath, "production": productionLookupPath,
		"isolation": isolationPath,
	}
	for name, target := range fixtures {
		payload := readCacheIsolationGolden(t, name)
		path := filepath.Join(root, target)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := loadDeploymentEvidence(root, "final-v5-contracts-v1.3")
	if err != nil {
		t.Fatalf("byte-exact v1.3 command output failed the consumer digest chain: %v", err)
	}
	if evidence.matrixDigest != "342d257718a58bf3e084c7d776f5dc169b6a9d67f5063f72caff5cc1d1694bad" ||
		evidence.isolationDigest != "6d6f281a43512ebe07bd53329c0e275e8c48d6a82587c731b7bde05a574e1712" ||
		evidence.productionDigest != "c959d611e01ff8d8c089477c932ff7e7adcc8f9555140396e5b1ef55512af6a4" {
		t.Fatalf("v1.3 consumer digests were not recomputed: %+v", evidence)
	}
}

func readCacheIsolationGolden(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "final-v5-cache-isolation", "testdata", "v1.3",
		name+".json.gz.base64")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return payload
}

func unsupportedRegistry(contractRelease string) finalv5profile.Registry {
	return finalv5profile.Registry{ContractRelease: contractRelease, Profiles: []finalv5profile.Profile{{
		ID: "profile-test", Alias: "test",
	}}}
}

func writeFixtureSupport(t *testing.T, fixture *deploymentEvidenceFixture,
	contractRelease string) {
	t.Helper()
	support := finalv5profile.ActivationSupport{
		SchemaVersion: 1, Record: finalv5profile.ActivationSupportRecord,
		ContractRelease: contractRelease, ProfileRegistrySHA256: strings.Repeat("c", 64),
		ActivationImplementationAvailable:    true,
		ActivationSmokeManifestSHA256:        strings.Repeat("d", 64),
		OutsideProductRouteMatrixSHA256:      fixture.matrixDigest,
		SemanticCacheIsolationEvidenceSHA256: fixture.isolationDigest,
		OutsideProductRouteMatrixStatus:      "pass",
		SemanticCacheIsolationStatus:         "pass",
		SemanticCacheCatalogBound:            true,
		Profiles: []finalv5profile.ProfileActivationSupport{{
			ProfileID: "profile-test", ProfileAlias: "test", Reason: "not activated",
		}},
	}
	payload, err := finalv5profile.EncodeActivationSupport(support)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.root, supportPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCommittedEnforcesDeploymentEvidenceReleaseAndLinks(t *testing.T) {
	const current = "final-v5-contracts-v1.4"
	t.Run("absent manifest permits retained previous-release evidence", func(t *testing.T) {
		fixture := newDeploymentEvidenceFixture(t, "final-v5-contracts-v1.3")
		if err := verifyCommitted(fixture.root, unsupportedRegistry(current)); err != nil {
			t.Fatalf("an absent manifest tried to consume retained evidence: %v", err)
		}
	})
	t.Run("manifest cannot consume previous-release evidence", func(t *testing.T) {
		fixture := newDeploymentEvidenceFixture(t, "final-v5-contracts-v1.3")
		writeFixtureSupport(t, fixture, current)
		if err := verifyCommitted(fixture.root, unsupportedRegistry(current)); err == nil {
			t.Fatal("a current-release support manifest consumed previous-release evidence")
		}
	})
	t.Run("manifest cannot consume an inconsistent digest chain", func(t *testing.T) {
		fixture := newDeploymentEvidenceFixture(t, current)
		fixture.isolation["outside_product_route_matrix_sha256"] = strings.Repeat("b", 64)
		fixture.isolationDigest = writeFixtureDocument(t, fixture.root, isolationPath,
			fixture.isolation)
		writeFixtureSupport(t, fixture, current)
		if err := verifyCommitted(fixture.root, unsupportedRegistry(current)); err == nil {
			t.Fatal("a support manifest consumed an inconsistent deployment evidence chain")
		}
	})
}

// The committed registry must be exactly what the committed manifest derives.
// Nothing may hand-edit a readiness state into the registry.
func TestCommittedRegistryMatchesTheManifest(t *testing.T) {
	registry := committedRegistry(t)
	byID, err := committedSupport(t).SupportedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	supported, eligible, routable := 0, 0, 0
	for _, profile := range registry.Profiles {
		derived, reason := finalv5profile.ActivationSupportFor(byID, profile.ID,
			profile.CatalogSHA256, profile.Closure.SHA256)
		if derived != profile.Status.ActivationSupported {
			t.Errorf("%s: registry activation_supported=%t, manifest derives %t (%s)",
				profile.Alias, profile.Status.ActivationSupported, derived, reason.Code)
		}
		if profile.Status.ActivationSupported {
			supported++
		}
		if profile.TargetedRunEligible {
			eligible++
		}
		if profile.Routable {
			routable++
		}
		// targeted_run_eligible and routable are derived, never stored opinions.
		if profile.TargetedRunEligible != profile.Status.TargetedRunEligible() {
			t.Errorf("%s: targeted_run_eligible is not derived", profile.Alias)
		}
		if profile.Routable != profile.Status.Routable() {
			t.Errorf("%s: routable is not derived", profile.Alias)
		}
	}
	if supported != 24 {
		t.Errorf("registry reports %d activation-supported profiles, want 24", supported)
	}
	if eligible != 24 {
		t.Errorf("registry reports %d targeted-run-eligible profiles, want 24", eligible)
	}
	if routable != 0 {
		t.Errorf("registry reports %d routable profiles, want 0", routable)
	}
}

// A profile is only routable once a targeted run has passed. An activation
// smoke must never be able to produce that state.
func TestActivationSmokeNeverImpliesTargetedValidation(t *testing.T) {
	for _, profile := range committedRegistry(t).Profiles {
		if profile.Status.TargetedValidationPassed {
			t.Errorf("%s claims targeted validation without a targeted run", profile.Alias)
		}
		if profile.Status.ActivationSmokePassed && profile.Status.TargetedValidationPassed {
			t.Errorf("%s promoted an activation smoke into a targeted validation", profile.Alias)
		}
	}
}

// Publication-eligible evidence belongs to a Campaign. Recycling it into a
// pilot readiness state would let Campaign output justify its own preconditions.
func TestPublicationEligibleEvidenceCannotSupportAProfile(t *testing.T) {
	profile := finalv5profile.Profile{ID: "profile-abc", Alias: "result-heavy",
		CatalogSHA256: strings.Repeat("a", 64),
		Closure:       finalv5profile.Closure{SHA256: strings.Repeat("b", 64)},
		Status: finalv5profile.ProfileStatus{ClosureComplete: true,
			CatalogMaterializable: true, LiveRouteAvailable: true}}
	passing := finalv5profile.ActivationEvidence{Status: "pass", ActivationSmokePassed: true,
		ContractRelease: "final-v5-contracts-v1.2", ProfileID: profile.ID,
		ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256}

	claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: passing}},
		"final-v5-contracts-v1.2", map[string]string{})
	if !claim.ActivationSupported {
		t.Fatal("a valid pilot evidence document did not support the profile")
	}

	for name, mutate := range map[string]func(*finalv5profile.ActivationEvidence){
		"publication eligible": func(e *finalv5profile.ActivationEvidence) { e.PublicationEligible = true },
		"failed status":        func(e *finalv5profile.ActivationEvidence) { e.Status = "fail" },
		"smoke not passed":     func(e *finalv5profile.ActivationEvidence) { e.ActivationSmokePassed = false },
		"different contract":   func(e *finalv5profile.ActivationEvidence) { e.ContractRelease = "final-v5-contracts-v1.1" },
		"different profile":    func(e *finalv5profile.ActivationEvidence) { e.ProfileID = "profile-other" },
		"different closure":    func(e *finalv5profile.ActivationEvidence) { e.ClosureSHA256 = strings.Repeat("d", 64) },
		"different catalog":    func(e *finalv5profile.ActivationEvidence) { e.CatalogSHA256 = strings.Repeat("e", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := passing
			mutate(&evidence)
			claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: evidence}},
				"final-v5-contracts-v1.2", map[string]string{})
			if claim.ActivationSupported || claim.ActivationSmokePassed {
				t.Fatalf("%s evidence supported the profile", name)
			}
			if !strings.Contains(claim.Reason, "live_activation_smoke_not_executed") {
				t.Fatalf("reason = %q", claim.Reason)
			}
		})
	}
}

// Passing evidence for a profile that is otherwise blocked must not promote it.
func TestEvidenceCannotPaperOverAnotherBlockedState(t *testing.T) {
	profile := finalv5profile.Profile{ID: "profile-abc", Alias: "analytics-orders",
		CatalogSHA256: strings.Repeat("a", 64),
		Closure:       finalv5profile.Closure{SHA256: strings.Repeat("b", 64)},
		Status: finalv5profile.ProfileStatus{ClosureComplete: true,
			CatalogMaterializable: true, LiveRouteAvailable: false}}
	passing := finalv5profile.ActivationEvidence{Status: "pass", ActivationSmokePassed: true,
		ContractRelease: "final-v5-contracts-v1.2", ProfileID: profile.ID,
		ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256}
	claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: passing}},
		"final-v5-contracts-v1.2", map[string]string{})
	if claim.ActivationSupported {
		t.Fatal("a profile with no live route was supported because evidence existed")
	}
	if !strings.Contains(claim.Reason, "live_route_available_false") {
		t.Fatalf("reason = %q", claim.Reason)
	}
}

// The manifest is committed to source control. It must carry no secret and no
// business data -- only identities and digests.
func TestCommittedManifestCarriesNoSecretsOrBusinessData(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), supportPath))
	if os.IsNotExist(err) {
		t.Skip("no activation support manifest for this contract release yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(payload))
	for _, forbidden := range []string{"postgres://", "password", "token", "bearer",
		"select ", "insert ", "task-", "parquet", "s3://", "dsn"} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("activation support manifest contains %q", forbidden)
		}
	}
	// Every digest-shaped value must be exactly a digest, never a payload.
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	for key, value := range document {
		if !strings.HasSuffix(key, "_sha256") {
			continue
		}
		text, ok := value.(string)
		if !ok || len(text) != 64 {
			t.Errorf("%s is not a bare SHA-256", key)
		}
	}
}
