// final-v5-observer is the source-controlled out-of-process telemetry probe
// used by the formal Final-V5 adapters. Docker Engine stats supply Gateway CPU
// and network counters. Docker Desktop does not expose cgroup memory.peak in
// that API, so a minimal read-only in-cgroup probe supplies memory.peak/events;
// phase ordering excludes that probe from the CPU/network delta, while the
// memory peak remains an explicitly probe-inclusive conservative upper bound.
// PostgreSQL counters come from read-only psql statements inside the two
// database containers. The observer never receives or prints credentials.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/formalbuild"
)

const (
	defaultDockerSocket = "/var/run/docker.sock"
	maxDockerResponse   = 1 << 20
	observerTimeout     = 10 * time.Second
	gatewayService      = "gateway"
	// businessCensusRole is the only role the Business census is restricted to.
	businessCensusRole = "gateway_reader"
	businessService    = "business-postgres"
	controlService     = "control-postgres"
	gatewayMemoryShell = `set -eu
printf 'TASKGATE_CGROUP\n'
cat /proc/self/cgroup
printf 'TASKGATE_CONTROLLERS\n'
cat /sys/fs/cgroup/cgroup.controllers
printf 'TASKGATE_MEMORY_PEAK\n'
cat /sys/fs/cgroup/memory.peak
printf 'TASKGATE_MEMORY_EVENTS\n'
cat /sys/fs/cgroup/memory.events`
	controlSnapshotShell = `set -eu
test -n "${POSTGRES_DB:-}"
psql -X --no-psqlrc --set ON_ERROR_STOP=1 --username postgres --dbname "$POSTGRES_DB" --tuples-only --no-align <<'TASKGATE_SQL'
SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint;
TASKGATE_SQL`
)

var (
	formalProjectPattern = regexp.MustCompile(`^taskgate-final-v5-deployment-0[1-3]-[0-9a-f]{20}$`)
	// The exact service set a formal Final-V5 deployment builds from
	// compose.yaml, compose.debug.yaml, compose.real-pilot.yaml and
	// compose.provsql.yaml. Every service remains named explicitly: an extra or
	// missing container invalidates the reading rather than being ignored.
	formalProjectServices = []string{
		businessService,
		controlService,
		"final-v5-direct-postgres",
		"final-v5-provsql-postgres",
		gatewayService,
		"oa-demo",
		"result-object-store",
		"result-object-store-init",
		"snapshot-index-detail",
		"snapshot-index-exposure-scale",
		"snapshot-index-result-heavy",
		"snapshot-index-scale-e7",
		"snapshot-index-summary",
		"snapshot-sidecar-install",
	}
)

// engine is everything the observer needs from Docker.
//
// It embeds formalbuild.Engine so the observer resolves the Gateway and
// PostgreSQL identities through the same inspection code the formal build path
// verifies with. The alternative -- accepting either identity from the Adapter
// or the environment -- would mean the finalizer's independence rested on a
// value supplied by the party being measured.
type engine interface {
	formalbuild.Engine
	resolveService(context.Context, string, string) (serviceIdentity, error)
	readGatewayResources(context.Context, serviceIdentity, string) (gatewayResourceSnapshot, error)
	exec(context.Context, string, []string) ([]byte, error)
	restartSnapshot(context.Context, string) (projectRestartSnapshot, error)
	resolveGatewayHealthcheck(context.Context, serviceIdentity) (experiment.GatewayHealthcheck, error)
}

type dockerEngine struct {
	client  *http.Client
	baseURL string
}

type containerSummary struct {
	ID string `json:"Id"`
}

type containerInspect struct {
	ID           string `json:"Id"`
	RestartCount int64  `json:"RestartCount"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
		// Healthcheck is the periodic probe Docker actually runs. It is read so
		// a formal measurement can refuse a deployment whose Gateway still
		// probes /health/ready, which would issue a full Business PostgreSQL
		// Attestation on every interval inside the observer window.
		Healthcheck *struct {
			Test     []string `json:"Test"`
			Interval int64    `json:"Interval"`
			Timeout  int64    `json:"Timeout"`
			Retries  int64    `json:"Retries"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
		PID     int  `json:"Pid"`
	} `json:"State"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

type execInspectResponse struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

type versionResponse struct {
	APIVersion string `json:"ApiVersion"`
}

type containerStatsResponse struct {
	ID       string `json:"id"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	Networks map[string]struct {
		RXBytes uint64 `json:"rx_bytes"`
		TXBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

type gatewayStatsSnapshot struct {
	cpuUsec, networkRX, networkTX int64
}

type gatewayResourceSnapshot struct {
	memoryPeak, memoryEvents      []byte
	cpuUsec, networkRX, networkTX int64
}

type serviceIdentity struct {
	project, service, id string
	pid                  int
}

type projectRestartSnapshot struct {
	counts   map[string]int64
	services map[string]string
	total    int64
}

func main() {
	if err := run(os.Args, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5 observer:", err)
		os.Exit(1)
	}
}

// run emits exactly one ObserverSnapshotV2.
//
// There is deliberately no fallback. A v1.5 invocation that cannot produce a
// complete v2 identity fails; it does not quietly emit the v1 document, because
// a path that degrades under exactly the conditions the evidence exists to
// detect is worse than no evidence at all.
func run(args []string, getenv func(string) string, stdout io.Writer) error {
	invocation, err := parseObserverInvocation(args)
	if err != nil {
		return err
	}
	project := strings.TrimSpace(getenv("COMPOSE_PROJECT_NAME"))
	if !formalProjectPattern.MatchString(project) {
		return errors.New("COMPOSE_PROJECT_NAME is not a formal Final-V5 deployment project")
	}
	observerSource, err := observerSourceSHA256()
	if err != nil {
		return err
	}
	// The expected source is computed from the repository, never read off the
	// image being verified. Materializing it here also means a dirty or
	// unpublished tree fails before any container is touched.
	expected, err := expectedGatewaySource(getenv)
	if err != nil {
		return err
	}
	socket, err := dockerSocket(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), observerTimeout)
	defer cancel()
	docker, err := newDockerEngine(socket)
	if err != nil {
		return err
	}
	if err := docker.negotiate(ctx); err != nil {
		return err
	}
	snapshot, err := collectV2(ctx, docker, project, invocation, observerSource, expected)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(snapshot)
}

// expectedGatewaySource materializes the published commit the running Gateway
// must have been built from.
//
// TASKGATE_FINAL_V5_REPO_ROOT selects the repository, not the identity: whatever
// root is named, the commit must be clean and published and the running image's
// labels must match what that commit materializes to. Naming the wrong root
// therefore fails the verification rather than passing it with different values.
func expectedGatewaySource(getenv func(string) string) (formalbuild.ExpectedSource, error) {
	root := strings.TrimSpace(getenv("TASKGATE_FINAL_V5_REPO_ROOT"))
	if root == "" {
		root = "."
	}
	materialized, err := formalbuild.MaterializeHead(root)
	if err != nil {
		return formalbuild.ExpectedSource{}, fmt.Errorf("materialize the Gateway source identity: %w", err)
	}
	return formalbuild.FromContext(materialized), nil
}

// collectV2 takes one atomic reading of everything the accounting depends on.
//
// The topology is read before and after the collection and required to be
// identical, so a container that restarted or was replaced mid-snapshot
// invalidates the reading rather than skewing it. The Business census is a
// single MATERIALIZED statement, so the role-wide total and the structural rows
// cannot describe two different instants -- the property
// ObserverSnapshotV2.Validate makes checkable by requiring the total to equal
// the sum of the rows.
func collectV2(ctx context.Context, docker engine, project string, invocation observerInvocation,
	observerSource string, expected formalbuild.ExpectedSource) (experiment.ObserverSnapshotV2, error) {
	var empty experiment.ObserverSnapshotV2
	if !formalProjectPattern.MatchString(project) {
		return empty, errors.New("invalid formal Compose project")
	}
	if invocation.phase != "before" && invocation.phase != "after" {
		return empty, errors.New("invalid observer phase")
	}
	restartsBefore, err := docker.restartSnapshot(ctx, project)
	if err != nil {
		return empty, fmt.Errorf("read project restart count: %w", err)
	}
	if err := validateFormalProjectSnapshot(restartsBefore); err != nil {
		return empty, err
	}
	gateway, err := docker.resolveService(ctx, project, gatewayService)
	if err != nil {
		return empty, fmt.Errorf("resolve Gateway container: %w", err)
	}
	business, err := docker.resolveService(ctx, project, businessService)
	if err != nil {
		return empty, fmt.Errorf("resolve Business PostgreSQL container: %w", err)
	}
	control, err := docker.resolveService(ctx, project, controlService)
	if err != nil {
		return empty, fmt.Errorf("resolve Control PostgreSQL container: %w", err)
	}
	if gateway.id == business.id || gateway.id == control.id || business.id == control.id {
		return empty, errors.New("formal services resolved to duplicate containers")
	}
	if !containsServiceIdentities(restartsBefore, gateway, business, control) {
		return empty, errors.New("required formal services are absent from the project container identity snapshot")
	}

	// The healthcheck is resolved before the census. A deployment whose Gateway
	// still probes /health/ready on a timer performs a full Business PostgreSQL
	// Attestation on every interval, so the window would carry a
	// wall-clock-dependent number of gateway_reader statements no derived plan
	// can predict; there is no point censusing it.
	healthcheck, err := docker.resolveGatewayHealthcheck(ctx, gateway)
	if err != nil {
		return empty, fmt.Errorf("resolve Gateway healthcheck: %w", err)
	}
	if err := healthcheck.Validate(); err != nil {
		return empty, err
	}
	gatewayIdentity, err := formalbuild.ResolveGatewayIdentity(ctx, docker, gateway.id, expected, healthcheck.SHA256())
	if err != nil {
		return empty, fmt.Errorf("resolve Gateway runtime identity: %w", err)
	}
	postgreSQL, err := formalbuild.ResolvePostgreSQLIdentity(ctx, docker, business.id)
	if err != nil {
		return empty, err
	}

	rawResources, err := docker.readGatewayResources(ctx, gateway, invocation.phase)
	if err != nil {
		return empty, err
	}
	resources, err := parseGatewayResources(rawResources)
	if err != nil {
		return empty, err
	}
	census, err := readBusinessCensus(ctx, docker, business.id)
	if err != nil {
		return empty, err
	}
	controlWAL, err := readControlWAL(ctx, docker, control.id)
	if err != nil {
		return empty, err
	}
	restartsAfter, err := docker.restartSnapshot(ctx, project)
	if err != nil {
		return empty, fmt.Errorf("re-read project restart count: %w", err)
	}
	if err := validateFormalProjectSnapshot(restartsAfter); err != nil {
		return empty, err
	}
	if !containsServiceIdentities(restartsAfter, gateway, business, control) ||
		!sameRestartSnapshot(restartsBefore, restartsAfter) {
		return empty, errors.New("a project container restarted or was replaced while the observer snapshot was collected")
	}
	projectTopology, err := runtimeIdentitySHA256(project, restartsAfter, healthcheck, gateway, business, control)
	if err != nil {
		return empty, err
	}

	snapshot := experiment.ObserverSnapshotV2{
		Version:                  experiment.ObserverSnapshotV2Version,
		SchemaVersion:            2,
		Phase:                    invocation.phase,
		ObserverWindowID:         invocation.observerWindowID,
		ClassifierManifestSHA256: invocation.classifierManifestSHA256,
		OperationBindingSHA256:   invocation.operationBindingSHA256,
		ObserverSourceSHA256:     observerSource,
		Label:                    invocation.phase,
		Role:                     businessCensusRole,
		Total:                    census.total,
		Structural:               census.rows,
		Environment:              census.environment,
		StatsReset:               census.statsReset,
		Dealloc:                  census.dealloc,
		Runtime: experiment.ObserverRuntimeIdentity{
			GatewayContainerID:    gateway.id,
			Gateway:               gatewayIdentity,
			PostgreSQL:            postgreSQL,
			ProjectTopologySHA256: projectTopology,
		},
		Resource: experiment.ObserverResourceEvidence{
			PostmasterStartTime:    census.postmasterStartTime,
			BusinessWALBytes:       census.walBytes,
			ControlWALBytes:        controlWAL,
			GatewayCPUUsec:         resources.cpuUsec,
			GatewayNetworkRXBytes:  resources.networkRX,
			GatewayNetworkTXBytes:  resources.networkTX,
			GatewayMemoryPeakBytes: resources.memoryPeak,
			GatewayRestartCount:    restartsAfter.counts[gateway.id],
			GatewayOOMKilled:       resources.oomEvents > 0,
			ContainerRestarts:      restartsAfter.total,
			OOMEvents:              resources.oomEvents,
		},
	}
	// Validated here rather than by the consumer: a snapshot that cannot serve as
	// authoritative evidence must never reach stdout, because once it is JSON on
	// a pipe it is indistinguishable from one that can.
	if err := snapshot.Validate(); err != nil {
		return empty, err
	}
	return snapshot, nil
}

func containsServiceIdentities(snapshot projectRestartSnapshot, identities ...serviceIdentity) bool {
	for _, identity := range identities {
		if identity.id == "" || identity.pid <= 0 || snapshot.services[identity.service] != identity.id {
			return false
		}
	}
	return true
}

func sameRestartSnapshot(left, right projectRestartSnapshot) bool {
	if left.total != right.total || len(left.counts) != len(right.counts) || len(left.services) != len(right.services) {
		return false
	}
	for containerID, count := range left.counts {
		if other, present := right.counts[containerID]; !present || other != count {
			return false
		}
	}
	for service, containerID := range left.services {
		if other, present := right.services[service]; !present || other != containerID {
			return false
		}
	}
	return true
}

func validateFormalProjectSnapshot(snapshot projectRestartSnapshot) error {
	if len(snapshot.services) != len(formalProjectServices) || len(snapshot.counts) != len(formalProjectServices) {
		return errors.New("formal Compose project service topology is not exact")
	}
	seenIDs := make(map[string]struct{}, len(formalProjectServices))
	var total int64
	for _, service := range formalProjectServices {
		containerID, present := snapshot.services[service]
		if !present || strings.TrimSpace(containerID) == "" {
			return errors.New("formal Compose project service topology is not exact")
		}
		if _, duplicate := seenIDs[containerID]; duplicate {
			return errors.New("formal Compose project service topology is not one-to-one")
		}
		seenIDs[containerID] = struct{}{}
		count, present := snapshot.counts[containerID]
		if !present || count < 0 || count > int64(^uint64(0)>>1)-total {
			return errors.New("formal Compose project restart topology is invalid")
		}
		total += count
	}
	if total != snapshot.total {
		return errors.New("formal Compose project restart total is inconsistent")
	}
	return nil
}

const runtimeIdentityDomain = "TASKGATE-FINAL-V5-OBSERVER-RUNTIME-IDENTITY-V1"

func runtimeIdentitySHA256(project string, snapshot projectRestartSnapshot,
	healthcheck experiment.GatewayHealthcheck, critical ...serviceIdentity) (string, error) {
	if !formalProjectPattern.MatchString(project) || !containsServiceIdentities(snapshot, critical...) {
		return "", errors.New("observer runtime identity input is incomplete")
	}
	if err := validateFormalProjectSnapshot(snapshot); err != nil {
		return "", err
	}
	services := make([]string, 0, len(snapshot.services))
	for service := range snapshot.services {
		services = append(services, service)
	}
	sort.Strings(services)
	var canonical strings.Builder
	canonical.WriteString(runtimeIdentityDomain)
	canonical.WriteByte(0)
	canonical.WriteString(project)
	canonical.WriteByte(0)
	for _, service := range services {
		canonical.WriteString(service)
		canonical.WriteByte(0)
		canonical.WriteString(snapshot.services[service])
		canonical.WriteByte(0)
	}
	// The periodic healthcheck is part of the runtime identity, so a snapshot
	// taken under the readiness probe can never compare equal to one taken
	// under the approved liveness probe.
	canonical.WriteString("gateway-healthcheck")
	canonical.WriteByte(0)
	canonical.WriteString(healthcheck.SHA256())
	canonical.WriteByte(0)
	canonical.WriteString("critical-pids")
	canonical.WriteByte(0)
	orderedCritical := append([]serviceIdentity(nil), critical...)
	sort.Slice(orderedCritical, func(i, j int) bool { return orderedCritical[i].service < orderedCritical[j].service })
	for _, identity := range orderedCritical {
		canonical.WriteString(identity.service)
		canonical.WriteByte(0)
		canonical.WriteString(strconv.Itoa(identity.pid))
		canonical.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(digest[:]), nil
}

type parsedGatewayResources struct {
	memoryPeak, cpuUsec, networkRX, networkTX, oomEvents int64
}

func parseGatewayResources(snapshot gatewayResourceSnapshot) (parsedGatewayResources, error) {
	// memory.peak is deliberately not reset. It is the kernel's cumulative
	// peak since this fresh Gateway cgroup was created, so each sample records
	// a deployment-cumulative upper bound that includes mmap-backed memory.
	memoryPeak, err := parsePositiveInt(snapshot.memoryPeak, "Gateway memory.peak")
	if err != nil {
		return parsedGatewayResources{}, err
	}
	memoryEvents, err := parseNamedCounters(snapshot.memoryEvents, "Gateway memory.events", []string{"oom"})
	if err != nil {
		return parsedGatewayResources{}, err
	}
	if snapshot.cpuUsec <= 0 || snapshot.networkRX < 0 || snapshot.networkTX < 0 {
		return parsedGatewayResources{}, errors.New("Docker Engine stats omitted a real Gateway CPU/network counter")
	}
	return parsedGatewayResources{memoryPeak: memoryPeak, cpuUsec: snapshot.cpuUsec,
		networkRX: snapshot.networkRX, networkTX: snapshot.networkTX, oomEvents: memoryEvents["oom"]}, nil
}

// ImageInspect, ContainerInspect, Exec and ResolveService make *dockerEngine a
// formalbuild.Engine, so the observer resolves the Gateway and PostgreSQL
// identities through the same code the formal build path is verified with
// rather than through a second, separately-drifting client.
func (d *dockerEngine) ImageInspect(ctx context.Context, reference string) (formalbuild.ImageInspect, error) {
	var inspected formalbuild.ImageInspect
	if strings.TrimSpace(reference) == "" {
		return inspected, errors.New("cannot inspect an image with no reference")
	}
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(reference)+"/json", &inspected); err != nil {
		return formalbuild.ImageInspect{}, fmt.Errorf("inspect image %s: %w", reference, err)
	}
	return inspected, nil
}

func (d *dockerEngine) ContainerInspect(ctx context.Context, containerID string) (formalbuild.ContainerInspect, error) {
	var inspected formalbuild.ContainerInspect
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/json", &inspected); err != nil {
		return formalbuild.ContainerInspect{}, err
	}
	if inspected.ID != containerID {
		return formalbuild.ContainerInspect{}, errors.New("Docker Engine returned a different container than the one inspected")
	}
	return inspected, nil
}

func (d *dockerEngine) Exec(ctx context.Context, containerID string, command []string) ([]byte, error) {
	return d.exec(ctx, containerID, command)
}

func (d *dockerEngine) ResolveService(ctx context.Context, project, service string) (string, error) {
	identity, err := d.resolveService(ctx, project, service)
	if err != nil {
		return "", err
	}
	return identity.id, nil
}

func readControlWAL(ctx context.Context, docker engine, containerID string) (int64, error) {
	raw, err := docker.exec(ctx, containerID, []string{"sh", "-c", controlSnapshotShell})
	if err != nil {
		return 0, fmt.Errorf("read Control PostgreSQL WAL counter: %w", err)
	}
	return parsePositiveString(strings.TrimSpace(string(raw)), "Control WAL position")
}

func parseGatewayMemoryProbe(raw []byte) ([]byte, []byte, error) {
	sections, err := parseSections(raw, []string{
		"TASKGATE_CGROUP", "TASKGATE_CONTROLLERS", "TASKGATE_MEMORY_PEAK", "TASKGATE_MEMORY_EVENTS",
	})
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(string(sections[0])) != "0::/" {
		return nil, nil, errors.New("Gateway is not in a private unified cgroup v2 namespace")
	}
	if !containsField(sections[1], "memory") || !containsField(sections[1], "cpu") {
		return nil, nil, errors.New("Gateway cgroup v2 memory or CPU controller is unavailable")
	}
	return sections[2], sections[3], nil
}

func parseSections(raw []byte, markers []string) ([][]byte, error) {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	result := make([][]byte, 0, len(markers))
	searchFrom := 0
	for index, marker := range markers {
		prefix := marker + "\n"
		relativeStart := strings.Index(normalized[searchFrom:], prefix)
		if relativeStart < 0 || strings.Count(normalized, prefix) != 1 {
			return nil, fmt.Errorf("Gateway snapshot marker %q is absent or duplicated", marker)
		}
		start := searchFrom + relativeStart + len(prefix)
		end := len(normalized)
		if index+1 < len(markers) {
			next := strings.Index(normalized[start:], markers[index+1]+"\n")
			if next < 0 {
				return nil, fmt.Errorf("Gateway snapshot marker %q is out of order", markers[index+1])
			}
			end = start + next
		}
		result = append(result, []byte(normalized[start:end]))
		searchFrom = end
	}
	return result, nil
}

func containsField(raw []byte, wanted string) bool {
	for _, field := range strings.Fields(string(raw)) {
		if field == wanted {
			return true
		}
	}
	return false
}

func parsePositiveInt(raw []byte, label string) (int64, error) {
	return parsePositiveString(strings.TrimSpace(string(raw)), label)
}

func parseNonnegativeString(raw, label string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s is not a nonnegative int64", label)
	}
	return value, nil
}

func parsePositiveString(raw, label string) (int64, error) {
	value, err := parseNonnegativeString(raw, label)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("%s is not a positive int64", label)
	}
	return value, nil
}

func parseNamedCounters(raw []byte, label string, required []string) (map[string]int64, error) {
	values := make(map[string]int64)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] == "" {
			return nil, fmt.Errorf("%s contains a malformed row", label)
		}
		if _, duplicate := values[fields[0]]; duplicate {
			return nil, fmt.Errorf("%s repeats %q", label, fields[0])
		}
		value, err := parseNonnegativeString(fields[1], label+" "+fields[0])
		if err != nil {
			return nil, err
		}
		values[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	for _, name := range required {
		if _, present := values[name]; !present {
			return nil, fmt.Errorf("%s omitted %q", label, name)
		}
	}
	return values, nil
}

func dockerSocket(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv("DOCKER_HOST"))
	if raw == "" {
		return defaultDockerSocket, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		!filepath.IsAbs(parsed.Path) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("DOCKER_HOST must be an absolute unix:// socket")
	}
	return filepath.Clean(parsed.Path), nil
}

func newDockerEngine(socket string) (*dockerEngine, error) {
	info, err := os.Stat(socket)
	if err != nil {
		return nil, fmt.Errorf("Docker socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Docker endpoint is not a Unix socket")
	}
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &dockerEngine{client: &http.Client{Transport: transport}, baseURL: "http://docker"}, nil
}

func (d *dockerEngine) negotiate(ctx context.Context) error {
	var version versionResponse
	if err := d.getJSON(ctx, "/version", &version); err != nil {
		return fmt.Errorf("negotiate Docker Engine API: %w", err)
	}
	if !validDockerAPIVersion(version.APIVersion) {
		return errors.New("Docker Engine returned an invalid API version")
	}
	d.baseURL = "http://docker/v" + version.APIVersion
	return nil
}

func validDockerAPIVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, one := range part {
			if one < '0' || one > '9' {
				return false
			}
		}
	}
	return true
}

func (d *dockerEngine) resolveService(ctx context.Context, project, service string) (serviceIdentity, error) {
	filters, err := json.Marshal(map[string][]string{"label": {
		"com.docker.compose.project=" + project,
		"com.docker.compose.service=" + service,
	}})
	if err != nil {
		return serviceIdentity{}, err
	}
	var containers []containerSummary
	if err := d.getJSON(ctx, "/containers/json?filters="+url.QueryEscape(string(filters)), &containers); err != nil {
		return serviceIdentity{}, err
	}
	if len(containers) != 1 || containers[0].ID == "" {
		return serviceIdentity{}, fmt.Errorf("found %d running containers for project=%q service=%q, want exactly one", len(containers), project, service)
	}
	return d.inspectServiceIdentity(ctx, containers[0].ID, project, service)
}

func (d *dockerEngine) inspectServiceIdentity(ctx context.Context, containerID, project, service string) (serviceIdentity, error) {
	var inspected containerInspect
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/json", &inspected); err != nil {
		return serviceIdentity{}, err
	}
	if inspected.ID != containerID || !inspected.State.Running || inspected.State.PID <= 0 ||
		inspected.Config.Labels["com.docker.compose.project"] != project ||
		inspected.Config.Labels["com.docker.compose.service"] != service {
		return serviceIdentity{}, errors.New("resolved container identity differs from its Compose labels or host PID")
	}
	return serviceIdentity{project: project, service: service, id: containerID, pid: inspected.State.PID}, nil
}

// resolveGatewayHealthcheck reads the running Gateway's periodic probe. Docker
// reports the interval and timeout in nanoseconds; they are converted to the
// same second-granularity strings the Compose override declares so the observer
// compares like with like.
func (d *dockerEngine) resolveGatewayHealthcheck(ctx context.Context, identity serviceIdentity) (experiment.GatewayHealthcheck, error) {
	var inspected containerInspect
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(identity.id)+"/json", &inspected); err != nil {
		return experiment.GatewayHealthcheck{}, err
	}
	if inspected.ID != identity.id {
		return experiment.GatewayHealthcheck{}, errors.New("Gateway container identity changed during healthcheck inspection")
	}
	if inspected.Config.Healthcheck == nil {
		return experiment.GatewayHealthcheck{}, errors.New("the running Gateway declares no periodic healthcheck")
	}
	probe := inspected.Config.Healthcheck
	return experiment.GatewayHealthcheck{
		Test:     append([]string(nil), probe.Test...),
		Interval: durationSeconds(probe.Interval),
		Timeout:  durationSeconds(probe.Timeout),
		Retries:  probe.Retries,
	}, nil
}

func durationSeconds(nanoseconds int64) string {
	if nanoseconds <= 0 || nanoseconds%int64(time.Second) != 0 {
		// Anything that is not a whole number of seconds cannot equal the
		// approved definition, and is reported verbatim so the mismatch is
		// legible rather than rounded away.
		return fmt.Sprintf("%dns", nanoseconds)
	}
	return fmt.Sprintf("%ds", nanoseconds/int64(time.Second))
}

func (d *dockerEngine) readGatewayResources(ctx context.Context, identity serviceIdentity, phase string) (gatewayResourceSnapshot, error) {
	before, err := d.inspectServiceIdentity(ctx, identity.id, identity.project, identity.service)
	if err != nil || before != identity {
		return gatewayResourceSnapshot{}, errors.New("Gateway container/PID changed before resource collection")
	}
	resources, err := bracketGatewayResources(phase, func() ([]byte, []byte, error) {
		raw, execErr := d.exec(ctx, identity.id, []string{"sh", "-c", gatewayMemoryShell})
		if execErr != nil {
			return nil, nil, fmt.Errorf("read Gateway cgroup memory counters: %w", execErr)
		}
		return parseGatewayMemoryProbe(raw)
	}, func() (gatewayStatsSnapshot, error) {
		return d.gatewayStats(ctx, identity.id)
	})
	if err != nil {
		return gatewayResourceSnapshot{}, err
	}
	after, err := d.inspectServiceIdentity(ctx, identity.id, identity.project, identity.service)
	if err != nil || after != identity {
		return gatewayResourceSnapshot{}, errors.New("Gateway container/PID changed during resource collection")
	}
	return resources, nil
}

func bracketGatewayResources(phase string, readMemory func() ([]byte, []byte, error),
	readStats func() (gatewayStatsSnapshot, error)) (gatewayResourceSnapshot, error) {
	var memoryPeak, memoryEvents []byte
	var stats gatewayStatsSnapshot
	var err error
	readMemoryOne := func() error {
		memoryPeak, memoryEvents, err = readMemory()
		return err
	}
	readStatsOne := func() error {
		stats, err = readStats()
		return err
	}
	switch phase {
	case "before":
		if err := readMemoryOne(); err != nil {
			return gatewayResourceSnapshot{}, err
		}
		if err := readStatsOne(); err != nil {
			return gatewayResourceSnapshot{}, err
		}
	case "after":
		if err := readStatsOne(); err != nil {
			return gatewayResourceSnapshot{}, err
		}
		if err := readMemoryOne(); err != nil {
			return gatewayResourceSnapshot{}, err
		}
	default:
		return gatewayResourceSnapshot{}, errors.New("invalid Gateway resource bracket phase")
	}
	return gatewayResourceSnapshot{memoryPeak: memoryPeak, memoryEvents: memoryEvents,
		cpuUsec: stats.cpuUsec, networkRX: stats.networkRX, networkTX: stats.networkTX}, nil
}

func (d *dockerEngine) gatewayStats(ctx context.Context, containerID string) (gatewayStatsSnapshot, error) {
	var response containerStatsResponse
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/stats?stream=false&one-shot=true", &response); err != nil {
		return gatewayStatsSnapshot{}, fmt.Errorf("read Docker Engine Gateway stats: %w", err)
	}
	return parseGatewayStats(containerID, response)
}

func parseGatewayStats(containerID string, response containerStatsResponse) (gatewayStatsSnapshot, error) {
	maxInt64 := uint64(^uint64(0) >> 1)
	if response.ID != containerID || response.CPUStats.CPUUsage.TotalUsage < 1000 ||
		response.CPUStats.CPUUsage.TotalUsage/1000 > maxInt64 || len(response.Networks) == 0 {
		return gatewayStatsSnapshot{}, errors.New("Docker Engine Gateway stats identity or CPU/network counters are invalid")
	}
	var rxTotal, txTotal uint64
	for name, network := range response.Networks {
		if strings.TrimSpace(name) == "" || network.RXBytes > maxInt64-rxTotal || network.TXBytes > maxInt64-txTotal {
			return gatewayStatsSnapshot{}, errors.New("Docker Engine Gateway network counter is invalid or overflows")
		}
		rxTotal += network.RXBytes
		txTotal += network.TXBytes
	}
	return gatewayStatsSnapshot{cpuUsec: int64(response.CPUStats.CPUUsage.TotalUsage / 1000),
		networkRX: int64(rxTotal), networkTX: int64(txTotal)}, nil
}

func (d *dockerEngine) restartSnapshot(ctx context.Context, project string) (projectRestartSnapshot, error) {
	filters, err := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	if err != nil {
		return projectRestartSnapshot{}, err
	}
	var containers []containerSummary
	if err := d.getJSON(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &containers); err != nil {
		return projectRestartSnapshot{}, err
	}
	if len(containers) == 0 {
		return projectRestartSnapshot{}, errors.New("formal Compose project has no containers")
	}
	result := projectRestartSnapshot{counts: make(map[string]int64), services: make(map[string]string)}
	for _, container := range containers {
		if container.ID == "" {
			return projectRestartSnapshot{}, errors.New("Docker returned an empty project container identity")
		}
		if _, duplicate := result.counts[container.ID]; duplicate {
			return projectRestartSnapshot{}, errors.New("Docker returned a duplicate project container identity")
		}
		var inspected containerInspect
		if err := d.getJSON(ctx, "/containers/"+url.PathEscape(container.ID)+"/json", &inspected); err != nil {
			return projectRestartSnapshot{}, err
		}
		service := strings.TrimSpace(inspected.Config.Labels["com.docker.compose.service"])
		if inspected.ID != container.ID || inspected.Config.Labels["com.docker.compose.project"] != project ||
			service == "" || inspected.RestartCount < 0 {
			return projectRestartSnapshot{}, errors.New("project container has an invalid Compose/restart identity")
		}
		if _, duplicate := result.services[service]; duplicate {
			return projectRestartSnapshot{}, errors.New("formal Compose project has duplicate service containers")
		}
		if inspected.RestartCount > int64(^uint64(0)>>1)-result.total {
			return projectRestartSnapshot{}, errors.New("project restart counter overflow")
		}
		result.counts[container.ID] = inspected.RestartCount
		result.services[service] = container.ID
		result.total += inspected.RestartCount
	}
	if err := validateFormalProjectSnapshot(result); err != nil {
		return projectRestartSnapshot{}, err
	}
	return result, nil
}

func (d *dockerEngine) exec(ctx context.Context, containerID string, command []string) ([]byte, error) {
	request := struct {
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Tty          bool     `json:"Tty"`
		Cmd          []string `json:"Cmd"`
	}{AttachStdout: true, AttachStderr: true, Tty: true, Cmd: command}
	var created execCreateResponse
	if err := d.postJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/exec", request, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("Docker Engine returned an empty exec id")
	}
	body, status, err := d.request(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]bool{"Detach": false, "Tty": true})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Docker exec start returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var inspected execInspectResponse
	if err := d.getJSON(ctx, "/exec/"+url.PathEscape(created.ID)+"/json", &inspected); err != nil {
		return nil, err
	}
	if inspected.Running {
		return nil, errors.New("Docker exec was still running after attached start completed")
	}
	if inspected.ExitCode != 0 {
		return nil, fmt.Errorf("container observer command exited %d: %s", inspected.ExitCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (d *dockerEngine) getJSON(ctx context.Context, path string, target any) error {
	body, status, err := d.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Docker Engine returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Docker Engine returned trailing JSON data")
	}
	return nil
}

func (d *dockerEngine) postJSON(ctx context.Context, path string, request, target any) error {
	body, status, err := d.request(ctx, http.MethodPost, path, request)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Docker Engine returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, target)
}

func (d *dockerEngine) request(ctx context.Context, method, path string, value any) ([]byte, int, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxDockerResponse+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxDockerResponse {
		return nil, 0, errors.New("Docker Engine response exceeded 1 MiB")
	}
	return raw, response.StatusCode, nil
}
