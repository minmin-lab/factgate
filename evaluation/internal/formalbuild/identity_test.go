package formalbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const (
	testCommit   = "0123456789abcdef0123456789abcdef01234567"
	testImageID  = "sha256:" + testContextDigest
	testOtherID  = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	testBuilder  = "golang@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testRuntime  = "debian@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testBinary   = "3333333333333333333333333333333333333333333333333333333333333333"
	testManifest = "4444444444444444444444444444444444444444444444444444444444444444"
)

const testContextDigest = "5555555555555555555555555555555555555555555555555555555555555555"

// fakeEngine answers from a recorded deployment so every rejection the formal
// path is supposed to make can be exercised without a Docker daemon. The checks
// are the load-bearing part; requiring a daemon to test them would mean they
// were never tested.
type fakeEngine struct {
	container ContainerInspect
	image     ImageInspect
	files     map[string]string
	binary    string
	execError error
}

func (engine *fakeEngine) ContainerInspect(_ context.Context, containerID string) (ContainerInspect, error) {
	if containerID != engine.container.ID {
		return ContainerInspect{}, fmt.Errorf("no such container %s", containerID)
	}
	return engine.container, nil
}

func (engine *fakeEngine) ImageInspect(_ context.Context, reference string) (ImageInspect, error) {
	if reference != engine.image.ID {
		return ImageInspect{}, fmt.Errorf("no such image %s", reference)
	}
	return engine.image, nil
}

func (engine *fakeEngine) ResolveService(_ context.Context, _, _ string) (string, error) {
	return engine.container.ID, nil
}

func (engine *fakeEngine) Exec(_ context.Context, _ string, command []string) ([]byte, error) {
	if engine.execError != nil {
		return nil, engine.execError
	}
	script := command[len(command)-1]
	if strings.Contains(script, "sha256sum") {
		return []byte(engine.binary + "\n"), nil
	}
	var rendered strings.Builder
	for _, path := range []string{
		sourceCommitPath, buildContextPath, sourceManifestPath, buildTargetPathInImg, binaryDigestPath,
	} {
		fmt.Fprintf(&rendered, "TASKGATE_FILE %s\n%s\n", path, engine.files[path])
	}
	return []byte(rendered.String()), nil
}

func healthcheckDigest(t *testing.T) string {
	t.Helper()
	digest := experiment.FormalGatewayHealthcheck().SHA256()
	if len(digest) != 64 {
		t.Fatalf("the formal healthcheck digest is %q", digest)
	}
	return digest
}

func newFakeEngine() *fakeEngine {
	engine := &fakeEngine{
		container: ContainerInspect{ID: "container0001", Image: testImageID},
		image: ImageInspect{
			ID: testImageID, Architecture: "amd64", OS: "linux",
		},
		binary: testBinary,
		files: map[string]string{
			sourceCommitPath:     testCommit,
			buildContextPath:     testContextDigest,
			sourceManifestPath:   testManifest,
			buildTargetPathInImg: GatewayBuildTarget,
			binaryDigestPath:     testBinary,
		},
	}
	engine.container.State.Running = true
	engine.image.Config.Labels = map[string]string{
		LabelFormalBuild:      "v1",
		LabelRevision:         testCommit,
		LabelBuildContext:     testContextDigest,
		LabelSourceManifest:   testManifest,
		LabelBuildTarget:      GatewayBuildTarget,
		LabelBuilderBaseImage: testBuilder,
		LabelRuntimeBaseImage: testRuntime,
	}
	return engine
}

func expectedSource() ExpectedSource {
	return ExpectedSource{
		Commit: testCommit, ContextSHA256: testContextDigest,
		SourceManifestSHA256: testManifest, CleanTree: true,
	}
}

func TestResolveGatewayIdentitySealsAnInspectableIdentity(t *testing.T) {
	engine := newFakeEngine()
	identity, err := ResolveGatewayIdentity(context.Background(), engine,
		engine.container.ID, expectedSource(), healthcheckDigest(t))
	if err != nil {
		t.Fatalf("ResolveGatewayIdentity: %v", err)
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("the sealed identity does not validate: %v", err)
	}

	// The load-bearing members must remain independently inspectable rather than
	// collapsing into the aggregate.
	for name, value := range map[string]string{
		"submission_commit":      identity.SubmissionCommit,
		"build_context_sha256":   identity.BuildContextSHA256,
		"source_manifest_sha256": identity.SourceManifestSHA256,
		"oci_revision_label":     identity.OCIRevisionLabel,
		"local_image_id":         identity.LocalImageID,
		"container_image_id":     identity.ContainerImageID,
		"gateway_binary_sha256":  identity.BinarySHA256,
		"platform":               identity.Platform,
		"builder_base_image":     identity.BuilderBaseImage,
		"runtime_base_image":     identity.RuntimeBaseImage,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s is empty; the identity is not independently inspectable", name)
		}
	}
	if identity.Platform != "linux/amd64" {
		t.Errorf("platform is %q, want linux/amd64", identity.Platform)
	}
	if identity.BuildTarget != GatewayBuildTarget {
		t.Errorf("build_target is %q", identity.BuildTarget)
	}
}

func TestResolveGatewayIdentityFailsClosed(t *testing.T) {
	healthcheck := healthcheckDigest(t)
	for name, corrupt := range map[string]func(*fakeEngine, *ExpectedSource){
		"image is not from the formal build path": func(engine *fakeEngine, _ *ExpectedSource) {
			delete(engine.image.Config.Labels, LabelFormalBuild)
		},
		"revision label names a different commit": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelRevision] = "fedcba9876543210fedcba9876543210fedcba98"
		},
		"build context label disagrees with the source": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelBuildContext] = testManifest
		},
		"source manifest label disagrees": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelSourceManifest] = testContextDigest
		},
		"image was built for another target": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelBuildTarget] = "final-v5-oracle"
			engine.files[buildTargetPathInImg] = "final-v5-oracle"
		},
		"a build argument was omitted": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelBuildContext] = ""
		},
		"a label is absent": func(engine *fakeEngine, _ *ExpectedSource) {
			delete(engine.image.Config.Labels, LabelBuilderBaseImage)
		},
		"container runs a different image": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.container.Image = testOtherID
		},
		"container is not running": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.container.State.Running = false
		},
		"the binary was replaced after the build": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.binary = "9999999999999999999999999999999999999999999999999999999999999999"
		},
		"the image was relabelled after it was built": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.files[sourceCommitPath] = "fedcba9876543210fedcba9876543210fedcba98"
		},
		"the image carries no formal provenance files": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.execError = errors.New("cat: no such file")
		},
		"the builder base image is not pinned": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelBuilderBaseImage] = "golang:1.25-bookworm"
		},
		"the runtime base image is not pinned": func(engine *fakeEngine, _ *ExpectedSource) {
			engine.image.Config.Labels[LabelRuntimeBaseImage] = "debian:bookworm-slim"
		},
		"the source was materialized from a dirty tree": func(_ *fakeEngine, expected *ExpectedSource) {
			expected.CleanTree = false
		},
		"the expected commit is abbreviated": func(_ *fakeEngine, expected *ExpectedSource) {
			expected.Commit = testCommit[:12]
		},
		"the expected context digest is missing": func(_ *fakeEngine, expected *ExpectedSource) {
			expected.ContextSHA256 = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			expected := expectedSource()
			corrupt(engine, &expected)
			if _, err := ResolveGatewayIdentity(context.Background(), engine,
				engine.container.ID, expected, healthcheck); err == nil {
				t.Fatal("the formal path accepted a Gateway it must reject before any snapshot is emitted")
			}
		})
	}
}

func TestResolveGatewayIdentityRejectsANonFormalHealthcheckDigest(t *testing.T) {
	engine := newFakeEngine()
	if _, err := ResolveGatewayIdentity(context.Background(), engine,
		engine.container.ID, expectedSource(), "not-a-digest"); err == nil {
		t.Fatal("an identity was sealed around an unresolved healthcheck")
	}
}

func TestGatewayRuntimeIdentityAggregateCoversEveryMember(t *testing.T) {
	engine := newFakeEngine()
	base, err := ResolveGatewayIdentity(context.Background(), engine,
		engine.container.ID, expectedSource(), healthcheckDigest(t))
	if err != nil {
		t.Fatalf("ResolveGatewayIdentity: %v", err)
	}

	for name, mutate := range map[string]func(*experiment.GatewayRuntimeIdentityV1){
		"submission_commit":      func(i *experiment.GatewayRuntimeIdentityV1) { i.SubmissionCommit = testCommit[:39] + "0" },
		"build_context_sha256":   func(i *experiment.GatewayRuntimeIdentityV1) { i.BuildContextSHA256 = testManifest },
		"source_manifest_sha256": func(i *experiment.GatewayRuntimeIdentityV1) { i.SourceManifestSHA256 = testContextDigest },
		"gateway_binary_sha256":  func(i *experiment.GatewayRuntimeIdentityV1) { i.BinarySHA256 = testManifest },
		"platform":               func(i *experiment.GatewayRuntimeIdentityV1) { i.Platform = "linux/arm64" },
		"builder_base_image":     func(i *experiment.GatewayRuntimeIdentityV1) { i.BuilderBaseImage = testRuntime },
		"runtime_base_image":     func(i *experiment.GatewayRuntimeIdentityV1) { i.RuntimeBaseImage = testBuilder },
		"healthcheck":            func(i *experiment.GatewayRuntimeIdentityV1) { i.HealthcheckSHA256 = testManifest },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			// The aggregate is deliberately left at its original value: a member
			// that changed without changing the aggregate is exactly the silent
			// substitution the aggregate exists to catch.
			if err := mutated.Validate(); err == nil {
				t.Fatalf("mutating %s left the identity valid; the aggregate does not cover it", name)
			}
		})
	}
}

func TestGatewayRuntimeIdentityRejectsADirtyBuildAndAMismatchedContainer(t *testing.T) {
	engine := newFakeEngine()
	identity, err := ResolveGatewayIdentity(context.Background(), engine,
		engine.container.ID, expectedSource(), healthcheckDigest(t))
	if err != nil {
		t.Fatalf("ResolveGatewayIdentity: %v", err)
	}

	dirty := identity
	dirty.CleanTreeAtBuild = false
	if resealed, err := dirty.Seal(); err == nil {
		t.Fatalf("an identity built from a dirty tree sealed cleanly: %+v", resealed)
	}

	swapped := identity
	swapped.ContainerImageID = testOtherID
	if _, err := swapped.Seal(); err == nil {
		t.Fatal("an identity whose container runs a different image than the build produced sealed cleanly")
	}
}

// --- base image pins ---------------------------------------------------------

func TestBaseImagePinsRefuseAnUnpinnedFormalBuild(t *testing.T) {
	pins := BaseImagePins{Version: BaseImagePinsVersion, Images: []BaseImagePin{
		{Role: BuilderRole, Tag: "golang:1.25-bookworm", Digest: ""},
		{Role: RuntimeRole, Tag: "debian:bookworm-slim", Digest: "sha256:" + testManifest},
	}}
	if err := pins.Validate(); err != nil {
		t.Fatalf("an unpinned document must still parse so it can be filled in: %v", err)
	}
	err := pins.Pinned()
	if err == nil {
		t.Fatal("a formal build accepted a base image pinned only by tag")
	}
	if !strings.Contains(err.Error(), "builder") {
		t.Fatalf("the failure does not name the unpinned role: %v", err)
	}
	if _, err := (BaseImagePin{Role: BuilderRole, Tag: "golang:1.25-bookworm"}).Reference(); err == nil {
		t.Fatal("an unpinned pin produced a reference")
	}
}

func TestBaseImagePinsValidateStructure(t *testing.T) {
	for name, pins := range map[string]BaseImagePins{
		"wrong version": {Version: "v0", Images: []BaseImagePin{
			{Role: BuilderRole, Tag: "a:1"}, {Role: RuntimeRole, Tag: "b:1"}}},
		"unknown role": {Version: BaseImagePinsVersion, Images: []BaseImagePin{
			{Role: "sidecar", Tag: "a:1"}, {Role: BuilderRole, Tag: "b:1"}, {Role: RuntimeRole, Tag: "c:1"}}},
		"duplicate role": {Version: BaseImagePinsVersion, Images: []BaseImagePin{
			{Role: BuilderRole, Tag: "a:1"}, {Role: BuilderRole, Tag: "b:1"}, {Role: RuntimeRole, Tag: "c:1"}}},
		"missing role": {Version: BaseImagePinsVersion, Images: []BaseImagePin{
			{Role: BuilderRole, Tag: "a:1"}}},
		"no tag": {Version: BaseImagePinsVersion, Images: []BaseImagePin{
			{Role: BuilderRole, Tag: ""}, {Role: RuntimeRole, Tag: "b:1"}}},
		"malformed digest": {Version: BaseImagePinsVersion, Images: []BaseImagePin{
			{Role: BuilderRole, Tag: "a:1", Digest: "sha256:zz"}, {Role: RuntimeRole, Tag: "b:1"}}},
	} {
		if err := pins.Validate(); err == nil {
			t.Errorf("%s: an invalid pin document validated", name)
		}
	}
}

func TestTrackedBaseImagePinDocumentIsReadable(t *testing.T) {
	// The committed document must always parse, whether or not it is pinned yet:
	// an unreadable pin file would fail the build for the wrong reason.
	pins, err := LoadBaseImagePins("../../../" + BaseImagePinPath)
	if err != nil {
		t.Fatalf("the tracked base image pin document does not load: %v", err)
	}
	for _, role := range []string{BuilderRole, RuntimeRole} {
		if _, err := pins.Pin(role); err != nil {
			t.Errorf("the tracked pin document names no %s image: %v", role, err)
		}
	}
}

// --- build arguments ---------------------------------------------------------

func pinnedPins() BaseImagePins {
	return BaseImagePins{Version: BaseImagePinsVersion, Images: []BaseImagePin{
		{Role: BuilderRole, Tag: "golang:1.25-bookworm", Digest: "sha256:" + testContextDigest},
		{Role: RuntimeRole, Tag: "debian:bookworm-slim", Digest: "sha256:" + testManifest},
	}}
}

func buildableContext() Context {
	return Context{
		Source:               SourceState{Commit: testCommit, Branch: "main", CleanTree: true, Published: true},
		Archive:              []byte("tar"),
		ContextSHA256:        testContextDigest,
		SourceManifestSHA256: testManifest,
	}
}

func TestBuildArgumentsPassEverySourceIdentity(t *testing.T) {
	arguments, err := BuildRequest{Context: buildableContext(), Pins: pinnedPins(), Tag: "t:formal"}.BuildArguments()
	if err != nil {
		t.Fatalf("BuildArguments: %v", err)
	}
	rendered := strings.Join(arguments, " ")
	for _, required := range []string{
		"--file " + FormalDockerfile,
		"SOURCE_COMMIT=" + testCommit,
		"BUILD_CONTEXT_SHA256=" + testContextDigest,
		"SOURCE_MANIFEST_SHA256=" + testManifest,
		"TARGET=" + GatewayBuildTarget,
		"BUILDER_BASE_IMAGE=golang@sha256:" + testContextDigest,
		"RUNTIME_BASE_IMAGE=debian@sha256:" + testManifest,
	} {
		if !strings.Contains(rendered, required) {
			t.Errorf("the formal build argv omits %q: %s", required, rendered)
		}
	}
	// The context must be streamed, never read from a host directory.
	if arguments[len(arguments)-1] != "-" {
		t.Errorf("the formal build reads a host directory instead of the materialized context: %v", arguments)
	}
}

func TestBuildArgumentsRefuseAnUnbuildableSource(t *testing.T) {
	for name, mutate := range map[string]func(*BuildRequest){
		"unpinned bases": func(request *BuildRequest) {
			request.Pins.Images[0].Digest = ""
		},
		"dirty tree": func(request *BuildRequest) {
			request.Context.Source.CleanTree = false
		},
		"unpublished commit": func(request *BuildRequest) {
			request.Context.Source.Published = false
		},
		"undigested context": func(request *BuildRequest) {
			request.Context.ContextSHA256 = ""
		},
		"no tag": func(request *BuildRequest) {
			request.Tag = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := BuildRequest{Context: buildableContext(), Pins: pinnedPins(), Tag: "t:formal"}
			mutate(&request)
			if _, err := request.BuildArguments(); err == nil {
				t.Fatal("the formal build accepted a source it must refuse")
			}
		})
	}
}

func TestRequireFormalImageRejectsAMislabelledBuild(t *testing.T) {
	engine := newFakeEngine()
	if err := RequireFormalImage(engine.image, expectedSource()); err != nil {
		t.Fatalf("a correctly labelled formal image was rejected: %v", err)
	}
	for name, corrupt := range map[string]func(*ImageInspect){
		"no formal label": func(image *ImageInspect) { delete(image.Config.Labels, LabelFormalBuild) },
		"wrong revision":  func(image *ImageInspect) { image.Config.Labels[LabelRevision] = testCommit[:39] + "f" },
		"wrong context":   func(image *ImageInspect) { image.Config.Labels[LabelBuildContext] = testManifest },
		"wrong target":    func(image *ImageInspect) { image.Config.Labels[LabelBuildTarget] = "oracle" },
		"missing label":   func(image *ImageInspect) { delete(image.Config.Labels, LabelSourceManifest) },
		"no platform":     func(image *ImageInspect) { image.OS, image.Architecture = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			image := newFakeEngine().image
			labels := map[string]string{}
			for key, value := range image.Config.Labels {
				labels[key] = value
			}
			image.Config.Labels = labels
			corrupt(&image)
			if err := RequireFormalImage(image, expectedSource()); err == nil {
				t.Fatal("a mislabelled image passed the post-build check")
			}
		})
	}
}

func TestContextDigestOfTheRealRepositoryIsStable(t *testing.T) {
	// Guards the property the whole path rests on: digesting the same commit
	// twice must agree, or no label could ever be verified.
	root := newRepository(t)
	first, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead: %v", err)
	}
	second, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead: %v", err)
	}
	if first.ContextSHA256 != second.ContextSHA256 {
		t.Fatal("materializing one commit twice produced two context digests")
	}
	sum := sha256.Sum256(first.Archive)
	if hex.EncodeToString(sum[:]) == "" {
		t.Fatal("the materialized archive is empty")
	}
}

func TestBuildArgumentsCarryTheModuleProxyOnlyWhenSet(t *testing.T) {
	base := BuildRequest{Context: buildableContext(), Pins: pinnedPins(), Tag: "t:formal"}
	arguments, err := base.BuildArguments()
	if err != nil {
		t.Fatalf("BuildArguments: %v", err)
	}
	if strings.Contains(strings.Join(arguments, " "), "GOPROXY=") {
		t.Fatalf("an unset module proxy reached the builder: %v", arguments)
	}
	base.ModuleProxy = "http://172.17.0.1:3000,https://proxy.golang.org,direct"
	arguments, err = base.BuildArguments()
	if err != nil {
		t.Fatalf("BuildArguments with a module proxy: %v", err)
	}
	rendered := strings.Join(arguments, " ")
	if !strings.Contains(rendered, "--build-arg GOPROXY="+base.ModuleProxy) {
		t.Fatalf("the module proxy was not passed as GOPROXY: %s", rendered)
	}
	if arguments[len(arguments)-1] != "-" {
		t.Fatalf("the module proxy displaced the streamed context: %v", arguments)
	}
	base.ModuleProxy = "http://172.17.0.1:3000 --network=host"
	if _, err := base.BuildArguments(); err == nil {
		t.Fatal("a module proxy carrying whitespace was accepted")
	}
}

func TestBuildArgumentsTargetDefaultsToTheGatewayAndAdmitsOnlyListedAuxiliaries(t *testing.T) {
	render := func(target string) (string, error) {
		arguments, err := BuildRequest{Context: buildableContext(), Pins: pinnedPins(), Tag: "t:formal", Target: target}.BuildArguments()
		return strings.Join(arguments, " "), err
	}
	for _, target := range []string{"", GatewayBuildTarget} {
		rendered, err := render(target)
		if err != nil || !strings.Contains(rendered, "TARGET="+GatewayBuildTarget+" ") {
			t.Fatalf("target %q: %v %s", target, err, rendered)
		}
	}
	rendered, err := render("oa-demo")
	if err != nil || !strings.Contains(rendered, "TARGET=oa-demo ") || strings.Contains(rendered, "TARGET=gateway") {
		t.Fatalf("oa-demo: %v %s", err, rendered)
	}
	for _, refused := range []string{"kb-publish", "gateway oa-demo", "../cmd/gateway", "OA-DEMO"} {
		if _, err := render(refused); err == nil {
			t.Errorf("target %q accepted", refused)
		}
	}
}

func TestRequireFormalImageForTargetChecksTheTargetLabel(t *testing.T) {
	expected := ExpectedSource{Commit: testCommit, ContextSHA256: testContextDigest, SourceManifestSHA256: testManifest}
	image := func(target string) ImageInspect {
		var inspect ImageInspect
		inspect.ID = "sha256:" + testContextDigest
		inspect.Architecture, inspect.OS = "amd64", "linux"
		inspect.Config.Labels = map[string]string{LabelFormalBuild: "v1", LabelRevision: testCommit,
			LabelBuildContext: testContextDigest, LabelSourceManifest: testManifest, LabelBuildTarget: target}
		return inspect
	}
	if err := RequireFormalImageForTarget(image("oa-demo"), expected, "oa-demo"); err != nil {
		t.Fatalf("oa-demo image refused: %v", err)
	}
	if err := RequireFormalImage(image("oa-demo"), expected); err == nil {
		t.Fatal("an oa-demo image passed as a Gateway image")
	}
	if err := RequireFormalImageForTarget(image(GatewayBuildTarget), expected, "oa-demo"); err == nil {
		t.Fatal("a Gateway image passed as an oa-demo image")
	}
}
