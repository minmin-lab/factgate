package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	hybridSetDigestDomain   = "TASKGATE-V4-HYBRID-SET-V1\x00"
	observationDigestDomain = "TASKGATE-V4-OBSERVATION-V1\x00"
	dynamicFactDomainV2     = "TASKGATE-FACT-V2\x00"
	dynamicFactDomainV3     = "TASKGATE-FACT-V3\x00"
)

// OrdinalRootHead is the durable three-dimensional V4 state. Set digests are
// published together by one epoch CAS while the root row is locked.
type OrdinalRootHead struct {
	RootTaskID          string
	ProfileVersion      string
	DictionarySetDigest string
	Epoch               int64
	Limits              ExposureLimits
	Used                ExposureLimits
	ReleaseSetSHA256    string
	InfluenceSetSHA256  string
	OutcomeSetSHA256    string
	UpdatedAt           time.Time
}

type normalizedOrdinalSet struct {
	static       ordinal.BitmapSet
	dynamic      []OrdinalDynamicFact
	digest       string
	staticCount  int64
	dynamicCount int64
}

func (s normalizedOrdinalSet) cardinality() int64 { return s.staticCount + s.dynamicCount }

type normalizedOrdinalObservation struct {
	dictionarySet string
	release       normalizedOrdinalSet
	influence     normalizedOrdinalSet
	outcome       normalizedOrdinalSet
	digest        string
}

func ensureOrdinalExposureHeadTx(ctx context.Context, tx *sql.Tx, taskID string, grant ExposureGrant, now time.Time) error {
	if grant.ProfileVersion != exposure.ProfileV4 {
		return fmt.Errorf("ordinal root head requires %s", exposure.ProfileV4)
	}
	if err := activateV4CutoverTx(ctx, tx, taskID, now); err != nil {
		return err
	}
	var rootTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM tasks WHERE id=$1 FOR SHARE`, taskID).Scan(&rootTaskID); err != nil {
		return err
	}
	var existing OrdinalRootHead
	err := tx.QueryRowContext(ctx, `
SELECT root_task_id,profile_version,COALESCE(dictionary_set_digest,''),epoch,
 max_release_facts,max_influence_facts,max_outcome_facts,
 used_release_facts,used_influence_facts,used_outcome_facts,
 COALESCE(release_set_sha256,''),COALESCE(influence_set_sha256,''),COALESCE(outcome_set_sha256,''),updated_at
FROM v4_exposure_root_heads WHERE root_task_id=$1 FOR UPDATE`, rootTaskID).
		Scan(&existing.RootTaskID, &existing.ProfileVersion, &existing.DictionarySetDigest, &existing.Epoch,
			&existing.Limits.ReleaseFacts, &existing.Limits.InfluenceFacts, &existing.Limits.OutcomeFacts,
			&existing.Used.ReleaseFacts, &existing.Used.InfluenceFacts, &existing.Used.OutcomeFacts,
			&existing.ReleaseSetSHA256, &existing.InfluenceSetSHA256, &existing.OutcomeSetSHA256, &existing.UpdatedAt)
	if err == nil {
		if existing.ProfileVersion != grant.ProfileVersion ||
			grant.Limits.ReleaseFacts > existing.Limits.ReleaseFacts ||
			grant.Limits.InfluenceFacts > existing.Limits.InfluenceFacts ||
			grant.Limits.OutcomeFacts > existing.Limits.OutcomeFacts {
			return fmt.Errorf("delegated V4 exposure grant expands or changes its root head")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	if rootTaskID != taskID {
		return fmt.Errorf("delegated task has no V4 root head")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO v4_exposure_root_heads(root_task_id,profile_version,max_release_facts,max_influence_facts,max_outcome_facts,updated_at)
VALUES ($1,$2,$3,$4,$5,$6)`, rootTaskID, grant.ProfileVersion, grant.Limits.ReleaseFacts,
		grant.Limits.InfluenceFacts, grant.Limits.OutcomeFacts, dbTime(now))
	return err
}

func getOrdinalExposureLedger(ctx context.Context, source rowQueryer, taskID string) (ExposureLedgerSnapshot, error) {
	var result ExposureLedgerSnapshot
	var updated time.Time
	err := source.QueryRowContext(ctx, `
SELECT h.root_task_id,h.profile_version,h.epoch,h.max_release_facts,h.max_influence_facts,h.max_outcome_facts,
 h.used_release_facts,h.used_influence_facts,h.used_outcome_facts,h.updated_at
FROM tasks t JOIN v4_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&result.RootTaskID, &result.ProfileVersion, &result.RootEpoch, &result.Limits.ReleaseFacts,
			&result.Limits.InfluenceFacts, &result.Limits.OutcomeFacts, &result.Used.ReleaseFacts,
			&result.Used.InfluenceFacts, &result.Used.OutcomeFacts, &updated)
	if err != nil {
		return ExposureLedgerSnapshot{}, err
	}
	result.UpdatedAt = dbTime(updated)
	return result, nil
}

func (s *Store) GetOrdinalRootHead(ctx context.Context, taskID string) (OrdinalRootHead, error) {
	const op = "get ordinal root head"
	if err := s.checkOpen(op); err != nil {
		return OrdinalRootHead{}, err
	}
	var head OrdinalRootHead
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT h.root_task_id,h.profile_version,COALESCE(h.dictionary_set_digest,''),h.epoch,
 h.max_release_facts,h.max_influence_facts,h.max_outcome_facts,
 h.used_release_facts,h.used_influence_facts,h.used_outcome_facts,
 COALESCE(h.release_set_sha256,''),COALESCE(h.influence_set_sha256,''),COALESCE(h.outcome_set_sha256,''),h.updated_at
FROM tasks t JOIN v4_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&head.RootTaskID, &head.ProfileVersion, &head.DictionarySetDigest, &head.Epoch,
			&head.Limits.ReleaseFacts, &head.Limits.InfluenceFacts, &head.Limits.OutcomeFacts,
			&head.Used.ReleaseFacts, &head.Used.InfluenceFacts, &head.Used.OutcomeFacts,
			&head.ReleaseSetSHA256, &head.InfluenceSetSHA256, &head.OutcomeSetSHA256, &updated)
	if err != nil {
		if isNoRows(err) {
			return OrdinalRootHead{}, opErr(op, ErrNotFound, err)
		}
		return OrdinalRootHead{}, opErr(op, ErrConflict, err)
	}
	head.UpdatedAt = dbTime(updated)
	return head, nil
}

func reserveOrdinalExposureTx(ctx context.Context, tx *sql.Tx, queryID, taskID string, request *ExposureReservationRequest, now time.Time) (*ExposureReservation, error) {
	var rootTaskID, profile string
	if err := tx.QueryRowContext(ctx, `SELECT t.root_task_id,h.profile_version FROM tasks t
JOIN v4_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&rootTaskID, &profile); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, ErrExposureEvidenceRequired
	}
	if request.ProfileVersion != profile || profile != exposure.ProfileV4 ||
		request.EstimatedReleaseFacts < 0 || request.EstimatedInfluenceFacts < 0 || request.EstimatedOutcomeFacts <= 0 {
		return nil, fmt.Errorf("invalid V4 exposure reservation")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO v4_query_exposure_reservations(query_id,task_id,root_task_id,profile_version,status,
 estimated_release_facts,estimated_influence_facts,estimated_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,'RESERVED',$5,$6,$7,$8)`, queryID, taskID, rootTaskID, profile,
		request.EstimatedReleaseFacts, request.EstimatedInfluenceFacts, request.EstimatedOutcomeFacts, dbTime(now))
	if err != nil {
		return nil, err
	}
	return &ExposureReservation{QueryID: queryID, TaskID: taskID, RootTaskID: rootTaskID,
		ProfileVersion: profile, EstimatedReleaseFacts: request.EstimatedReleaseFacts,
		EstimatedInfluenceFacts: request.EstimatedInfluenceFacts,
		EstimatedOutcomeFacts:   request.EstimatedOutcomeFacts}, nil
}

func settleAnyExposureMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, settlement BudgetSettlement) (*ExposureCharge, exposureSettlementMetrics, error) {
	var isV4, isV5 bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS (SELECT 1 FROM v4_query_exposure_reservations WHERE query_id=$1),
 EXISTS (SELECT 1 FROM v5_query_exposure_reservations WHERE query_id=$1)`, settlement.QueryID).Scan(&isV4, &isV5); err != nil {
		return nil, exposureSettlementMetrics{}, err
	}
	if isV5 {
		if (settlement.OrdinalExposure == nil) == (settlement.OrdinalObservationRef == nil) || settlement.Exposure != nil {
			return nil, exposureSettlementMetrics{}, errors.New("V5 requires exactly one observation or committed observation reference")
		}
		if settlement.OrdinalObservationRef != nil {
			return settleV5ObservationRefMeasuredTx(ctx, tx, now, settlement.QueryID, settlement.OrdinalObservationRef)
		}
		return settleV5ExposureMeasuredTx(ctx, tx, now, settlement.QueryID, settlement.OrdinalExposure)
	}
	if isV4 {
		if (settlement.OrdinalExposure == nil) == (settlement.OrdinalObservationRef == nil) {
			if settlement.OrdinalExposure == nil {
				return nil, exposureSettlementMetrics{}, ErrExposureEvidenceRequired
			}
			return nil, exposureSettlementMetrics{}, fmt.Errorf("V4 observation and observation reference are mutually exclusive")
		}
		if settlement.Exposure != nil {
			return nil, exposureSettlementMetrics{}, fmt.Errorf("legacy and V4 exposure evidence are mutually exclusive")
		}
		if settlement.OrdinalObservationRef != nil {
			return settleOrdinalObservationRefMeasuredTx(ctx, tx, now, settlement.QueryID, settlement.OrdinalObservationRef)
		}
		if settlement.OrdinalExposure == nil {
			return nil, exposureSettlementMetrics{}, ErrExposureEvidenceRequired
		}
		return settleOrdinalExposureMeasuredTx(ctx, tx, now, settlement.QueryID, settlement.OrdinalExposure)
	}
	if settlement.OrdinalExposure != nil || settlement.OrdinalObservationRef != nil {
		return nil, exposureSettlementMetrics{}, fmt.Errorf("V4 exposure evidence supplied without a V4 reservation")
	}
	return settleExposureMeasuredTx(ctx, tx, now, settlement.QueryID, settlement.Exposure)
}

// settleOrdinalObservationRefMeasuredTx is the true semantic replay path. It
// never parses, persists, loads, unions, or differences a bitmap. PostgreSQL's
// FK-visible root_observations row proves that the observation and the root
// head transition committed together; monotonic root ledgers make its novelty
// permanently zero for that root.
func settleOrdinalObservationRefMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string,
	reference *OrdinalObservationReference) (*ExposureCharge, exposureSettlementMetrics, error) {
	var metrics exposureSettlementMetrics
	if reference == nil || !validSHA256Hex(reference.ObservationSHA256) || !validSHA256Hex(reference.DictionarySetDigest) {
		return nil, metrics, fmt.Errorf("invalid V4 observation reference")
	}
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	var storedEpoch int64
	started := time.Now()
	err := tx.QueryRowContext(ctx, `
SELECT query_id,task_id,root_task_id,profile_version,estimated_release_facts,estimated_influence_facts,
 estimated_outcome_facts,status,actual_release_facts,actual_influence_facts,actual_outcome_facts,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,observation_sha256,root_epoch
FROM v4_query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &reservation.EstimatedOutcomeFacts,
			&status, &actual.ReleaseFacts, &actual.InfluenceFacts, &actual.OutcomeFacts,
			&charged.ReleaseFacts, &charged.InfluenceFacts, &charged.OutcomeFacts, &storedDigest, &storedEpoch)
	metrics.ReservationLock = time.Since(started)
	if err != nil {
		return nil, metrics, err
	}
	var profile, dictionarySet, releaseSet, influenceSet, outcomeSet string
	var setCatalog, queryCatalog string
	var releaseCount, influenceCount, outcomeCount, firstEpoch, rootEpoch int64
	var headProfile, headDictionary string
	// No root-head lock is needed here: a visible immutable root_observations
	// row was inserted by the same transaction as the original head update.
	// PostgreSQL statement snapshots cannot expose it before that transaction
	// commits, and the root fact sets only grow afterwards.
	err = tx.QueryRowContext(ctx, `
SELECT observation.profile_version,observation.dictionary_set_digest,
 observation.release_set_sha256,observation.influence_set_sha256,observation.outcome_set_sha256,
 observation.actual_release_facts,observation.actual_influence_facts,observation.actual_outcome_facts,
 seen.first_epoch,head.epoch,head.profile_version,COALESCE(head.dictionary_set_digest,''),
 dictionary_set.catalog_digest,query.catalog_digest
FROM v4_observations observation
JOIN v4_root_observations seen ON seen.observation_sha256=observation.observation_sha256
JOIN v4_exposure_root_heads head ON head.root_task_id=seen.root_task_id
JOIN v4_dictionary_sets dictionary_set ON dictionary_set.dictionary_set_digest=observation.dictionary_set_digest
JOIN query_records query ON query.id=$3
WHERE seen.root_task_id=$1 AND observation.observation_sha256=$2`,
		reservation.RootTaskID, reference.ObservationSHA256, queryID).
		Scan(&profile, &dictionarySet, &releaseSet, &influenceSet, &outcomeSet,
			&releaseCount, &influenceCount, &outcomeCount, &firstEpoch, &rootEpoch, &headProfile, &headDictionary,
			&setCatalog, &queryCatalog)
	if isNoRows(err) {
		return nil, metrics, fmt.Errorf("observation %s is not committed for root %s", reference.ObservationSHA256, reservation.RootTaskID)
	}
	if err != nil {
		return nil, metrics, err
	}
	if profile != exposure.ProfileV4 || headProfile != exposure.ProfileV4 ||
		dictionarySet != reference.DictionarySetDigest || headDictionary != dictionarySet ||
		setCatalog != queryCatalog || outcomeCount != 1 || firstEpoch > rootEpoch {
		return nil, metrics, fmt.Errorf("observation reference dictionary/profile mismatch")
	}
	charge := &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
		ProfileVersion: exposure.ProfileV4, ActualReleaseFacts: releaseCount,
		ActualInfluenceFacts: influenceCount, ActualOutcomeFacts: outcomeCount,
		ObservationSHA256: reference.ObservationSHA256, DictionarySetDigest: dictionarySet,
		ReleaseSetSHA256: releaseSet, InfluenceSetSHA256: influenceSet, OutcomeSetSHA256: outcomeSet,
		RootEpoch: rootEpoch}
	if status == exposureSettled {
		if storedDigest != reference.ObservationSHA256 || actual.ReleaseFacts != releaseCount ||
			actual.InfluenceFacts != influenceCount || actual.OutcomeFacts != outcomeCount {
			return nil, metrics, fmt.Errorf("query already settled with different V4 exposure evidence")
		}
		charge.ChargedReleaseFacts = charged.ReleaseFacts
		charge.ChargedInfluenceFacts = charged.InfluenceFacts
		charge.ChargedOutcomeFacts = charged.OutcomeFacts
		charge.RootEpoch = storedEpoch
		return charge, metrics, nil
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("V4 exposure reservation is %s", status)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO v4_query_observations(query_id,root_task_id,observation_sha256,root_epoch,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,0,0,0,$5)`, queryID, reservation.RootTaskID,
		reference.ObservationSHA256, rootEpoch, dbTime(now))
	if err != nil {
		return nil, metrics, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE v4_query_exposure_reservations SET status='SETTLED',actual_release_facts=$1,
 actual_influence_facts=$2,actual_outcome_facts=$3,charged_release_facts=0,
 charged_influence_facts=0,charged_outcome_facts=0,observation_sha256=$4,root_epoch=$5,settled_at=$6
WHERE query_id=$7 AND status='RESERVED'`, releaseCount, influenceCount, outcomeCount,
		reference.ObservationSHA256, rootEpoch, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, fmt.Errorf("V4 replay reservation changed concurrently")
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: reservation.TaskID, QueryID: queryID,
		Actor: "system", EventType: "QUERY_ORDINAL_SEMANTIC_REPLAY", OccurredAt: now,
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID,
			"profile_version": exposure.ProfileV4, "dictionary_set_digest": dictionarySet,
			"observation_sha256": reference.ObservationSHA256, "release_set_sha256": releaseSet,
			"influence_set_sha256": influenceSet, "outcome_set_sha256": outcomeSet,
			"actual_release_facts": releaseCount, "actual_influence_facts": influenceCount,
			"actual_outcome_facts": outcomeCount, "charged_release_facts": 0,
			"charged_influence_facts": 0, "charged_outcome_facts": 0, "root_epoch": rootEpoch,
			"first_observation_epoch": firstEpoch})})
	if err != nil {
		return nil, metrics, err
	}
	return charge, metrics, nil
}

func normalizeOrdinalObservationTx(ctx context.Context, tx *sql.Tx, queryID string, observation *OrdinalExposureObservation, now time.Time) (normalizedOrdinalObservation, error) {
	if observation == nil || observation.ProfileVersion != exposure.ProfileV4 || !validSHA256Hex(observation.DictionarySetDigest) {
		return normalizedOrdinalObservation{}, fmt.Errorf("invalid V4 observation identity")
	}
	var setCatalog, queryCatalog string
	err := tx.QueryRowContext(ctx, `SELECT dictionary_set.catalog_digest,query.catalog_digest
FROM v4_dictionary_sets dictionary_set CROSS JOIN query_records query
WHERE dictionary_set.dictionary_set_digest=$1 AND query.id=$2`, observation.DictionarySetDigest, queryID).
		Scan(&setCatalog, &queryCatalog)
	if isNoRows(err) {
		return normalizedOrdinalObservation{}, fmt.Errorf("unknown V4 dictionary set %s", observation.DictionarySetDigest)
	}
	if err != nil {
		return normalizedOrdinalObservation{}, err
	}
	if setCatalog != queryCatalog {
		return normalizedOrdinalObservation{}, fmt.Errorf("V4 dictionary set Catalog does not match the query")
	}
	release, err := normalizeOrdinalSetTx(ctx, tx, queryID, observation.DictionarySetDigest,
		observation.Release, OrdinalDynamicDerivedRelease, map[string]bool{"BASE_CELL": true}, now)
	if err != nil {
		return normalizedOrdinalObservation{}, fmt.Errorf("release set: %w", err)
	}
	influence, err := normalizeOrdinalSetTx(ctx, tx, queryID, observation.DictionarySetDigest,
		observation.Influence, "", map[string]bool{"BASE_ROW": true, "BASE_CELL": true}, now)
	if err != nil {
		return normalizedOrdinalObservation{}, fmt.Errorf("influence set: %w", err)
	}
	outcome, err := normalizeOrdinalSetTx(ctx, tx, queryID, observation.DictionarySetDigest,
		observation.Outcome, OrdinalDynamicOutcome, nil, now)
	if err != nil {
		return normalizedOrdinalObservation{}, fmt.Errorf("outcome set: %w", err)
	}
	if outcome.staticCount != 0 || outcome.dynamicCount != 1 {
		return normalizedOrdinalObservation{}, fmt.Errorf("V4 requires exactly one dynamic outcome fact")
	}
	normalized := normalizedOrdinalObservation{dictionarySet: observation.DictionarySetDigest,
		release: release, influence: influence, outcome: outcome}
	normalized.digest = ordinalObservationDigest(normalized)
	if observation.ObservationSHA256 != "" && observation.ObservationSHA256 != normalized.digest {
		return normalizedOrdinalObservation{}, fmt.Errorf("V4 observation digest mismatch")
	}
	return normalized, nil
}

func normalizeOrdinalSetTx(ctx context.Context, tx *sql.Tx, queryID, dictionarySet string, set OrdinalHybridSet,
	allowedDynamicKind string, allowedStaticKinds map[string]bool, now time.Time) (normalizedOrdinalSet, error) {
	if len(allowedStaticKinds) == 0 && !set.Static.IsEmpty() {
		return normalizedOrdinalSet{}, fmt.Errorf("static ordinals are not allowed")
	}
	if err := validateOrdinalBoundsTx(ctx, tx, dictionarySet, set.Static, allowedStaticKinds); err != nil {
		return normalizedOrdinalSet{}, err
	}
	dynamic, err := normalizeDynamicFacts(set.DynamicFacts, allowedDynamicKind)
	if err != nil {
		return normalizedOrdinalSet{}, err
	}
	if err := persistDynamicFactsTx(ctx, tx, queryID, dictionarySet, dynamic, now); err != nil {
		return normalizedOrdinalSet{}, err
	}
	staticCount, err := ordinalCardinalityInt64(set.Static.Cardinality())
	if err != nil {
		return normalizedOrdinalSet{}, err
	}
	if uint64(len(dynamic)) > uint64(math.MaxInt64-staticCount) {
		return normalizedOrdinalSet{}, fmt.Errorf("hybrid set cardinality exceeds int64")
	}
	result := normalizedOrdinalSet{static: set.Static, dynamic: dynamic, staticCount: staticCount, dynamicCount: int64(len(dynamic))}
	result.digest, err = ordinalHybridSetDigest(dictionarySet, result.static, result.dynamic)
	return result, err
}

func validateOrdinalBoundsTx(ctx context.Context, tx *sql.Tx, dictionarySet string, set ordinal.BitmapSet,
	allowedKinds map[string]bool) error {
	for _, bound := range set.SegmentBounds() {
		var count int64
		var kind string
		err := tx.QueryRowContext(ctx, `
SELECT segment.ordinal_count,segment.fact_kind
FROM v4_dictionary_set_members member
JOIN v4_dictionary_segments segment ON segment.dictionary_digest=member.dictionary_digest
WHERE member.dictionary_set_digest=$1 AND segment.dictionary_digest=$2 AND segment.segment_id=$3`,
			dictionarySet, bound.Segment.DictionaryDigest, bound.Segment.SegmentID).Scan(&count, &kind)
		if isNoRows(err) {
			return fmt.Errorf("unknown dictionary segment %s/%s", bound.Segment.DictionaryDigest, bound.Segment.SegmentID)
		}
		if err != nil {
			return err
		}
		if count <= 0 || uint64(bound.MaxOrdinal) >= uint64(count) {
			return fmt.Errorf("ordinal %d is outside segment %s/%s cardinality %d", bound.MaxOrdinal,
				bound.Segment.DictionaryDigest, bound.Segment.SegmentID, count)
		}
		if !allowedKinds[kind] {
			return fmt.Errorf("segment %s/%s kind %s is invalid for this exposure dimension",
				bound.Segment.DictionaryDigest, bound.Segment.SegmentID, kind)
		}
	}
	return nil
}

func normalizeDynamicFacts(facts []OrdinalDynamicFact, allowedKind string) ([]OrdinalDynamicFact, error) {
	if allowedKind == "" && len(facts) != 0 {
		return nil, fmt.Errorf("dynamic facts are not allowed")
	}
	result := make([]OrdinalDynamicFact, 0, len(facts))
	byHash := make(map[string]OrdinalDynamicFact, len(facts))
	for _, fact := range facts {
		if !validSHA256Hex(fact.SHA256) || fact.Kind != allowedKind || len(fact.CanonicalPayload) == 0 {
			return nil, fmt.Errorf("invalid dynamic fact")
		}
		domain := dynamicFactDomainV2
		if fact.Kind == OrdinalDynamicOutcome {
			domain = dynamicFactDomainV3
		}
		material := make([]byte, 0, len(domain)+len(fact.CanonicalPayload))
		material = append(material, domain...)
		material = append(material, fact.CanonicalPayload...)
		digest := sha256.Sum256(material)
		if hex.EncodeToString(digest[:]) != fact.SHA256 {
			return nil, fmt.Errorf("dynamic fact hash does not commit its canonical payload")
		}
		fact.CanonicalPayload = append([]byte(nil), fact.CanonicalPayload...)
		if previous, duplicate := byHash[fact.SHA256]; duplicate {
			if previous.Kind != fact.Kind || !bytes.Equal(previous.CanonicalPayload, fact.CanonicalPayload) {
				return nil, fmt.Errorf("dynamic fact hash collision for %s", fact.SHA256)
			}
			continue
		}
		byHash[fact.SHA256] = fact
		result = append(result, fact)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SHA256 < result[j].SHA256 })
	return result, nil
}

func persistDynamicFactsTx(ctx context.Context, tx *sql.Tx, queryID, dictionarySet string, facts []OrdinalDynamicFact, now time.Time) error {
	for _, fact := range facts {
		result, err := tx.ExecContext(ctx, `
INSERT INTO v4_dynamic_facts(fact_sha256,fact_kind,canonical_payload,first_dictionary_set_digest,first_query_id,first_seen_at)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (fact_sha256) DO NOTHING`,
			fact.SHA256, fact.Kind, fact.CanonicalPayload, dictionarySet, queryID, dbTime(now))
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			var kind string
			var payload []byte
			if err := tx.QueryRowContext(ctx, `SELECT fact_kind,canonical_payload FROM v4_dynamic_facts
WHERE fact_sha256=$1`, fact.SHA256).Scan(&kind, &payload); err != nil {
				return err
			}
			if kind != fact.Kind || !bytes.Equal(payload, fact.CanonicalPayload) {
				return fmt.Errorf("dynamic fact hash collision for %s", fact.SHA256)
			}
		}
	}
	return nil
}

func persistOrdinalSetTx(ctx context.Context, tx *sql.Tx, dictionarySet string, set normalizedOrdinalSet, now time.Time) error {
	containers, err := set.static.PortableContainers()
	if err != nil {
		return err
	}
	for _, container := range containers {
		result, err := tx.ExecContext(ctx, `
INSERT INTO v4_bitmap_containers(container_sha256,dictionary_digest,segment_id,high16,cardinality,portable_payload,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (container_sha256) DO NOTHING`, container.Digest,
			container.Key.DictionaryDigest, container.Key.SegmentID, int(container.Key.High16),
			int64(container.Cardinality), container.Bitmap, dbTime(now))
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			var dictionary, segment string
			var high int
			var cardinality int64
			var payload []byte
			if err := tx.QueryRowContext(ctx, `SELECT dictionary_digest,segment_id,high16,cardinality,portable_payload
FROM v4_bitmap_containers WHERE container_sha256=$1`, container.Digest).
				Scan(&dictionary, &segment, &high, &cardinality, &payload); err != nil {
				return err
			}
			if dictionary != container.Key.DictionaryDigest || segment != container.Key.SegmentID ||
				high != int(container.Key.High16) || cardinality != int64(container.Cardinality) ||
				!bytes.Equal(payload, container.Bitmap) {
				return fmt.Errorf("bitmap container digest collision for %s", container.Digest)
			}
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO v4_bitmap_sets(set_sha256,dictionary_set_digest,static_cardinality,dynamic_cardinality,created_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (set_sha256) DO NOTHING`, set.digest, dictionarySet,
		set.staticCount, set.dynamicCount, dbTime(now))
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var storedDictionary string
		var staticCount, dynamicCount int64
		if err := tx.QueryRowContext(ctx, `SELECT dictionary_set_digest,static_cardinality,dynamic_cardinality
FROM v4_bitmap_sets WHERE set_sha256=$1`, set.digest).Scan(&storedDictionary, &staticCount, &dynamicCount); err != nil {
			return err
		}
		if storedDictionary != dictionarySet || staticCount != set.staticCount || dynamicCount != set.dynamicCount {
			return fmt.Errorf("bitmap set digest collision for %s", set.digest)
		}
	}
	for _, container := range containers {
		result, err = tx.ExecContext(ctx, `
INSERT INTO v4_bitmap_set_containers(set_sha256,dictionary_digest,segment_id,high16,container_sha256,cardinality)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (set_sha256,dictionary_digest,segment_id,high16) DO NOTHING`,
			set.digest, container.Key.DictionaryDigest, container.Key.SegmentID, int(container.Key.High16),
			container.Digest, int64(container.Cardinality))
		if err != nil {
			return err
		}
		inserted, _ = result.RowsAffected()
		if inserted == 0 {
			var digest string
			var cardinality int64
			if err := tx.QueryRowContext(ctx, `SELECT container_sha256,cardinality FROM v4_bitmap_set_containers
WHERE set_sha256=$1 AND dictionary_digest=$2 AND segment_id=$3 AND high16=$4`, set.digest,
				container.Key.DictionaryDigest, container.Key.SegmentID, int(container.Key.High16)).Scan(&digest, &cardinality); err != nil {
				return err
			}
			if digest != container.Digest || cardinality != int64(container.Cardinality) {
				return fmt.Errorf("bitmap set container collision for %s", set.digest)
			}
		}
	}
	for _, fact := range set.dynamic {
		result, err = tx.ExecContext(ctx, `
INSERT INTO v4_bitmap_set_dynamic_facts(set_sha256,fact_sha256)
VALUES ($1,$2) ON CONFLICT (set_sha256,fact_sha256) DO NOTHING`, set.digest, fact.SHA256)
		if err != nil {
			return err
		}
		_, _ = result.RowsAffected()
	}
	var containerCount, dynamicCount int
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM v4_bitmap_set_containers WHERE set_sha256=$1),
 (SELECT count(*) FROM v4_bitmap_set_dynamic_facts WHERE set_sha256=$1)`, set.digest).
		Scan(&containerCount, &dynamicCount); err != nil {
		return err
	}
	if containerCount != len(containers) || dynamicCount != len(set.dynamic) {
		return fmt.Errorf("bitmap set %s has different membership", set.digest)
	}
	return nil
}

func persistOrdinalObservationTx(ctx context.Context, tx *sql.Tx, normalized normalizedOrdinalObservation, now time.Time) error {
	for _, set := range []normalizedOrdinalSet{normalized.release, normalized.influence, normalized.outcome} {
		if err := persistOrdinalSetTx(ctx, tx, normalized.dictionarySet, set, now); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO v4_observations(observation_sha256,profile_version,dictionary_set_digest,
 release_set_sha256,influence_set_sha256,outcome_set_sha256,
 actual_release_facts,actual_influence_facts,actual_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (observation_sha256) DO NOTHING`,
		normalized.digest, exposure.ProfileV4, normalized.dictionarySet, normalized.release.digest,
		normalized.influence.digest, normalized.outcome.digest, normalized.release.cardinality(),
		normalized.influence.cardinality(), normalized.outcome.cardinality(), dbTime(now))
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var profile, dictionarySet, release, influence, outcome string
		var releaseCount, influenceCount, outcomeCount int64
		if err := tx.QueryRowContext(ctx, `SELECT profile_version,dictionary_set_digest,release_set_sha256,
influence_set_sha256,outcome_set_sha256,actual_release_facts,actual_influence_facts,actual_outcome_facts
FROM v4_observations WHERE observation_sha256=$1`, normalized.digest).
			Scan(&profile, &dictionarySet, &release, &influence, &outcome,
				&releaseCount, &influenceCount, &outcomeCount); err != nil {
			return err
		}
		if profile != exposure.ProfileV4 || dictionarySet != normalized.dictionarySet || release != normalized.release.digest ||
			influence != normalized.influence.digest || outcome != normalized.outcome.digest ||
			releaseCount != normalized.release.cardinality() || influenceCount != normalized.influence.cardinality() ||
			outcomeCount != normalized.outcome.cardinality() {
			return fmt.Errorf("observation digest collision for %s", normalized.digest)
		}
	}
	return nil
}

func loadOrdinalSetTx(ctx context.Context, tx *sql.Tx, setDigest string) (normalizedOrdinalSet, string, error) {
	var dictionarySet string
	var staticCount, dynamicCount int64
	if err := tx.QueryRowContext(ctx, `SELECT dictionary_set_digest,static_cardinality,dynamic_cardinality
FROM v4_bitmap_sets WHERE set_sha256=$1`, setDigest).Scan(&dictionarySet, &staticCount, &dynamicCount); err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT mapping.dictionary_digest,mapping.segment_id,mapping.high16,mapping.cardinality,
 container.container_sha256,container.dictionary_digest,container.segment_id,container.high16,
 container.cardinality,container.portable_payload
FROM v4_bitmap_set_containers mapping
JOIN v4_bitmap_containers container ON container.container_sha256=mapping.container_sha256
WHERE mapping.set_sha256=$1
ORDER BY mapping.dictionary_digest,mapping.segment_id,mapping.high16`, setDigest)
	if err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	var containers []ordinal.PortableContainer
	for rows.Next() {
		var mapDictionary, mapSegment, digest, blobDictionary, blobSegment string
		var mapHigh, blobHigh int
		var mapCardinality, blobCardinality int64
		var payload []byte
		if err := rows.Scan(&mapDictionary, &mapSegment, &mapHigh, &mapCardinality, &digest,
			&blobDictionary, &blobSegment, &blobHigh, &blobCardinality, &payload); err != nil {
			rows.Close()
			return normalizedOrdinalSet{}, "", err
		}
		if mapDictionary != blobDictionary || mapSegment != blobSegment || mapHigh != blobHigh ||
			mapCardinality != blobCardinality || mapHigh < 0 || mapHigh > math.MaxUint16 || mapCardinality <= 0 {
			rows.Close()
			return normalizedOrdinalSet{}, "", fmt.Errorf("corrupt bitmap container mapping for set %s", setDigest)
		}
		containers = append(containers, ordinal.PortableContainer{Key: ordinal.ContainerKey{
			DictionaryDigest: mapDictionary, SegmentID: mapSegment, High16: uint16(mapHigh)},
			Bitmap: payload, Cardinality: uint64(mapCardinality), Digest: digest})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return normalizedOrdinalSet{}, "", err
	}
	if err := rows.Close(); err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	static, err := ordinal.ParsePortableContainers(containers)
	if err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	dynamicRows, err := tx.QueryContext(ctx, `
SELECT fact.fact_sha256,fact.fact_kind,fact.canonical_payload
FROM v4_bitmap_set_dynamic_facts member
JOIN v4_dynamic_facts fact ON fact.fact_sha256=member.fact_sha256
WHERE member.set_sha256=$1 ORDER BY fact.fact_sha256`, setDigest)
	if err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	var dynamic []OrdinalDynamicFact
	for dynamicRows.Next() {
		var fact OrdinalDynamicFact
		if err := dynamicRows.Scan(&fact.SHA256, &fact.Kind, &fact.CanonicalPayload); err != nil {
			dynamicRows.Close()
			return normalizedOrdinalSet{}, "", err
		}
		dynamic = append(dynamic, fact)
	}
	if err := dynamicRows.Err(); err != nil {
		dynamicRows.Close()
		return normalizedOrdinalSet{}, "", err
	}
	if err := dynamicRows.Close(); err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	actualStatic, err := ordinalCardinalityInt64(static.Cardinality())
	if err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	if actualStatic != staticCount || int64(len(dynamic)) != dynamicCount {
		return normalizedOrdinalSet{}, "", fmt.Errorf("bitmap set %s cardinality is corrupt", setDigest)
	}
	if dynamicCount > math.MaxInt64-staticCount {
		return normalizedOrdinalSet{}, "", fmt.Errorf("bitmap set %s cardinality overflows", setDigest)
	}
	computed, err := ordinalHybridSetDigest(dictionarySet, static, dynamic)
	if err != nil {
		return normalizedOrdinalSet{}, "", err
	}
	if computed != setDigest {
		return normalizedOrdinalSet{}, "", fmt.Errorf("bitmap set %s content digest mismatch", setDigest)
	}
	return normalizedOrdinalSet{static: static, dynamic: dynamic, digest: setDigest,
		staticCount: staticCount, dynamicCount: dynamicCount}, dictionarySet, nil
}

func settleOrdinalExposureMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string,
	observation *OrdinalExposureObservation) (*ExposureCharge, exposureSettlementMetrics, error) {
	var metrics exposureSettlementMetrics
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	var storedEpoch int64
	reservationLockStarted := time.Now()
	err := tx.QueryRowContext(ctx, `
SELECT query_id,task_id,root_task_id,profile_version,estimated_release_facts,estimated_influence_facts,
 estimated_outcome_facts,status,actual_release_facts,actual_influence_facts,actual_outcome_facts,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,observation_sha256,root_epoch
FROM v4_query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &reservation.EstimatedOutcomeFacts,
			&status, &actual.ReleaseFacts, &actual.InfluenceFacts, &actual.OutcomeFacts,
			&charged.ReleaseFacts, &charged.InfluenceFacts, &charged.OutcomeFacts, &storedDigest, &storedEpoch)
	metrics.ReservationLock = time.Since(reservationLockStarted)
	if err != nil {
		if isNoRows(err) {
			return nil, metrics, fmt.Errorf("V4 exposure evidence supplied without a reservation")
		}
		return nil, metrics, err
	}
	normalized, err := normalizeOrdinalObservationTx(ctx, tx, queryID, observation, now)
	if err != nil {
		return nil, metrics, err
	}
	if status == exposureSettled {
		if storedDigest != normalized.digest {
			return nil, metrics, fmt.Errorf("query already settled with different V4 exposure evidence")
		}
		return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
			ProfileVersion: exposure.ProfileV4, ActualReleaseFacts: actual.ReleaseFacts,
			ActualInfluenceFacts: actual.InfluenceFacts, ActualOutcomeFacts: actual.OutcomeFacts,
			ChargedReleaseFacts: charged.ReleaseFacts, ChargedInfluenceFacts: charged.InfluenceFacts,
			ChargedOutcomeFacts: charged.OutcomeFacts, ObservationSHA256: storedDigest,
			DictionarySetDigest: normalized.dictionarySet, ReleaseSetSHA256: normalized.release.digest,
			InfluenceSetSHA256: normalized.influence.digest, OutcomeSetSHA256: normalized.outcome.digest,
			RootEpoch: storedEpoch}, metrics, nil
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("V4 exposure reservation is %s", status)
	}
	factStoreStarted := time.Now()
	if err := persistOrdinalObservationTx(ctx, tx, normalized, now); err != nil {
		return nil, metrics, err
	}
	metrics.FactStore = time.Since(factStoreStarted)

	var head OrdinalRootHead
	// Read the immutable head reference without locking it. The conditional
	// UPDATE below is the single three-dimensional linearization point. A lost
	// race returns ErrOrdinalCASConflict; the caller rolls back this transaction,
	// reloads the new head, and recomputes all three novelty deltas together.
	ledgerLockStarted := time.Now()
	err = tx.QueryRowContext(ctx, `
SELECT root_task_id,profile_version,COALESCE(dictionary_set_digest,''),epoch,
 max_release_facts,max_influence_facts,max_outcome_facts,
 used_release_facts,used_influence_facts,used_outcome_facts,
 COALESCE(release_set_sha256,''),COALESCE(influence_set_sha256,''),COALESCE(outcome_set_sha256,''),updated_at
	FROM v4_exposure_root_heads WHERE root_task_id=$1`, reservation.RootTaskID).
		Scan(&head.RootTaskID, &head.ProfileVersion, &head.DictionarySetDigest, &head.Epoch,
			&head.Limits.ReleaseFacts, &head.Limits.InfluenceFacts, &head.Limits.OutcomeFacts,
			&head.Used.ReleaseFacts, &head.Used.InfluenceFacts, &head.Used.OutcomeFacts,
			&head.ReleaseSetSHA256, &head.InfluenceSetSHA256, &head.OutcomeSetSHA256, &head.UpdatedAt)
	metrics.LedgerLock = time.Since(ledgerLockStarted)
	if err != nil {
		return nil, metrics, err
	}
	if head.ProfileVersion != exposure.ProfileV4 ||
		(head.DictionarySetDigest != "" && head.DictionarySetDigest != normalized.dictionarySet) {
		return nil, metrics, fmt.Errorf("V4 root head dictionary/profile mismatch")
	}
	var seen bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM v4_root_observations
WHERE root_task_id=$1 AND observation_sha256=$2)`, reservation.RootTaskID, normalized.digest).Scan(&seen); err != nil {
		return nil, metrics, err
	}
	rootEpoch := head.Epoch
	if seen {
		// The observation may have committed between the head read and the EXISTS
		// statement. Re-read the epoch so the per-query reference never predates
		// the observation's atomic root transition.
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM v4_exposure_root_heads WHERE root_task_id=$1`,
			reservation.RootTaskID).Scan(&rootEpoch); err != nil {
			return nil, metrics, err
		}
	}
	newRelease, newInfluence, newOutcome := int64(0), int64(0), int64(0)
	if !seen {
		empty, _ := ordinal.NewBitmapSet()
		rootRelease := normalizedOrdinalSet{static: empty}
		rootInfluence := normalizedOrdinalSet{static: empty}
		rootOutcome := normalizedOrdinalSet{static: empty}
		if head.ReleaseSetSHA256 != "" {
			var dictionary string
			rootRelease, dictionary, err = loadOrdinalSetTx(ctx, tx, head.ReleaseSetSHA256)
			if err != nil || dictionary != normalized.dictionarySet {
				if err == nil {
					err = fmt.Errorf("release root set dictionary mismatch")
				}
				return nil, metrics, err
			}
			rootInfluence, dictionary, err = loadOrdinalSetTx(ctx, tx, head.InfluenceSetSHA256)
			if err != nil || dictionary != normalized.dictionarySet {
				if err == nil {
					err = fmt.Errorf("influence root set dictionary mismatch")
				}
				return nil, metrics, err
			}
			rootOutcome, dictionary, err = loadOrdinalSetTx(ctx, tx, head.OutcomeSetSHA256)
			if err != nil || dictionary != normalized.dictionarySet {
				if err == nil {
					err = fmt.Errorf("outcome root set dictionary mismatch")
				}
				return nil, metrics, err
			}
		}
		deltaRelease, err := differenceOrdinalSet(normalized.dictionarySet, normalized.release, rootRelease)
		if err != nil {
			return nil, metrics, err
		}
		deltaInfluence, err := differenceOrdinalSet(normalized.dictionarySet, normalized.influence, rootInfluence)
		if err != nil {
			return nil, metrics, err
		}
		deltaOutcome, err := differenceOrdinalSet(normalized.dictionarySet, normalized.outcome, rootOutcome)
		if err != nil {
			return nil, metrics, err
		}
		newRelease, newInfluence, newOutcome = deltaRelease.cardinality(), deltaInfluence.cardinality(), deltaOutcome.cardinality()
		var taskLimits ExposureLimits
		if err := tx.QueryRowContext(ctx, `SELECT max_release_facts,max_influence_facts,max_outcome_facts
FROM task_grants WHERE task_id=$1`, reservation.TaskID).
			Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts, &taskLimits.OutcomeFacts); err != nil {
			return nil, metrics, err
		}
		if exceedsOrdinalLimit(head.Used, head.Limits, taskLimits, ExposureLimits{
			ReleaseFacts: newRelease, InfluenceFacts: newInfluence, OutcomeFacts: newOutcome}) {
			return nil, metrics, ErrExposureBudgetExhausted
		}
		if newRelease != 0 || newInfluence != 0 || newOutcome != 0 {
			mergedRelease, err := unionOrdinalSet(normalized.dictionarySet, rootRelease, normalized.release)
			if err != nil {
				return nil, metrics, err
			}
			mergedInfluence, err := unionOrdinalSet(normalized.dictionarySet, rootInfluence, normalized.influence)
			if err != nil {
				return nil, metrics, err
			}
			mergedOutcome, err := unionOrdinalSet(normalized.dictionarySet, rootOutcome, normalized.outcome)
			if err != nil {
				return nil, metrics, err
			}
			for _, set := range []normalizedOrdinalSet{mergedRelease, mergedInfluence, mergedOutcome} {
				if err := persistOrdinalSetTx(ctx, tx, normalized.dictionarySet, set, now); err != nil {
					return nil, metrics, err
				}
			}
			result, err := tx.ExecContext(ctx, `
UPDATE v4_exposure_root_heads SET dictionary_set_digest=$1,epoch=epoch+1,
 used_release_facts=used_release_facts+$2,used_influence_facts=used_influence_facts+$3,
 used_outcome_facts=used_outcome_facts+$4,release_set_sha256=$5,influence_set_sha256=$6,
 outcome_set_sha256=$7,updated_at=$8
WHERE root_task_id=$9 AND epoch=$10
 AND used_release_facts+$2 <= max_release_facts
 AND used_influence_facts+$3 <= max_influence_facts
 AND used_outcome_facts+$4 <= max_outcome_facts`, normalized.dictionarySet, newRelease, newInfluence,
				newOutcome, mergedRelease.digest, mergedInfluence.digest, mergedOutcome.digest, dbTime(now),
				reservation.RootTaskID, head.Epoch)
			if err != nil {
				return nil, metrics, err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return nil, metrics, ErrOrdinalCASConflict
			}
			rootEpoch = head.Epoch + 1
		}
		result, err := tx.ExecContext(ctx, `
INSERT INTO v4_root_observations(root_task_id,observation_sha256,first_query_id,first_epoch,first_seen_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (root_task_id,observation_sha256) DO NOTHING`, reservation.RootTaskID, normalized.digest, queryID, rootEpoch, dbTime(now))
		if err != nil {
			return nil, metrics, err
		}
		if inserted, _ := result.RowsAffected(); inserted == 0 {
			// This is possible only for a concurrent zero-novelty observation; a
			// non-zero contender must first win the epoch CAS. Use the committed
			// head epoch for this query reference.
			if newRelease != 0 || newInfluence != 0 || newOutcome != 0 {
				return nil, metrics, ErrOrdinalCASConflict
			}
			if err := tx.QueryRowContext(ctx, `SELECT epoch FROM v4_exposure_root_heads WHERE root_task_id=$1`,
				reservation.RootTaskID).Scan(&rootEpoch); err != nil {
				return nil, metrics, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO v4_query_observations(query_id,root_task_id,observation_sha256,root_epoch,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, queryID, reservation.RootTaskID, normalized.digest, rootEpoch,
		newRelease, newInfluence, newOutcome, dbTime(now))
	if err != nil {
		return nil, metrics, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE v4_query_exposure_reservations SET status='SETTLED',actual_release_facts=$1,
 actual_influence_facts=$2,actual_outcome_facts=$3,charged_release_facts=$4,
 charged_influence_facts=$5,charged_outcome_facts=$6,observation_sha256=$7,root_epoch=$8,settled_at=$9
WHERE query_id=$10 AND status='RESERVED'`, normalized.release.cardinality(), normalized.influence.cardinality(),
		normalized.outcome.cardinality(), newRelease, newInfluence, newOutcome, normalized.digest,
		rootEpoch, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, fmt.Errorf("V4 exposure reservation changed concurrently")
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: reservation.TaskID, QueryID: queryID,
		Actor: "system", EventType: "QUERY_ORDINAL_EXPOSURE_SETTLED", OccurredAt: now,
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID,
			"profile_version": exposure.ProfileV4, "dictionary_set_digest": normalized.dictionarySet,
			"actual_release_facts":   normalized.release.cardinality(),
			"actual_influence_facts": normalized.influence.cardinality(),
			"actual_outcome_facts":   normalized.outcome.cardinality(),
			"charged_release_facts":  newRelease, "charged_influence_facts": newInfluence,
			"charged_outcome_facts": newOutcome, "observation_sha256": normalized.digest,
			"release_set_sha256": normalized.release.digest, "influence_set_sha256": normalized.influence.digest,
			"outcome_set_sha256": normalized.outcome.digest, "root_epoch": rootEpoch})})
	if err != nil {
		return nil, metrics, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
		ProfileVersion: exposure.ProfileV4, ActualReleaseFacts: normalized.release.cardinality(),
		ActualInfluenceFacts: normalized.influence.cardinality(), ActualOutcomeFacts: normalized.outcome.cardinality(),
		ChargedReleaseFacts: newRelease, ChargedInfluenceFacts: newInfluence, ChargedOutcomeFacts: newOutcome,
		ObservationSHA256: normalized.digest, DictionarySetDigest: normalized.dictionarySet,
		ReleaseSetSHA256: normalized.release.digest, InfluenceSetSHA256: normalized.influence.digest,
		OutcomeSetSHA256: normalized.outcome.digest, RootEpoch: rootEpoch}, metrics, nil
}

func differenceOrdinalSet(dictionarySet string, left, right normalizedOrdinalSet) (normalizedOrdinalSet, error) {
	dynamicRight := make(map[string]struct{}, len(right.dynamic))
	for _, fact := range right.dynamic {
		dynamicRight[fact.SHA256] = struct{}{}
	}
	dynamic := make([]OrdinalDynamicFact, 0, len(left.dynamic))
	for _, fact := range left.dynamic {
		if _, exists := dynamicRight[fact.SHA256]; !exists {
			dynamic = append(dynamic, fact)
		}
	}
	static := left.static.Difference(right.static)
	staticCount, err := ordinalCardinalityInt64(static.Cardinality())
	if err != nil {
		return normalizedOrdinalSet{}, err
	}
	if uint64(len(dynamic)) > uint64(math.MaxInt64-staticCount) {
		return normalizedOrdinalSet{}, fmt.Errorf("hybrid set cardinality exceeds int64")
	}
	result := normalizedOrdinalSet{static: static, dynamic: dynamic, staticCount: staticCount, dynamicCount: int64(len(dynamic))}
	result.digest, err = ordinalHybridSetDigest(dictionarySet, static, dynamic)
	return result, err
}

func unionOrdinalSet(dictionarySet string, left, right normalizedOrdinalSet) (normalizedOrdinalSet, error) {
	byHash := make(map[string]OrdinalDynamicFact, len(left.dynamic)+len(right.dynamic))
	for _, source := range [][]OrdinalDynamicFact{left.dynamic, right.dynamic} {
		for _, fact := range source {
			if previous, exists := byHash[fact.SHA256]; exists &&
				(previous.Kind != fact.Kind || !bytes.Equal(previous.CanonicalPayload, fact.CanonicalPayload)) {
				return normalizedOrdinalSet{}, fmt.Errorf("dynamic fact hash collision for %s", fact.SHA256)
			}
			byHash[fact.SHA256] = fact
		}
	}
	dynamic := make([]OrdinalDynamicFact, 0, len(byHash))
	for _, fact := range byHash {
		dynamic = append(dynamic, fact)
	}
	sort.Slice(dynamic, func(i, j int) bool { return dynamic[i].SHA256 < dynamic[j].SHA256 })
	static := left.static.Union(right.static)
	staticCount, err := ordinalCardinalityInt64(static.Cardinality())
	if err != nil {
		return normalizedOrdinalSet{}, err
	}
	if uint64(len(dynamic)) > uint64(math.MaxInt64-staticCount) {
		return normalizedOrdinalSet{}, fmt.Errorf("hybrid set cardinality exceeds int64")
	}
	result := normalizedOrdinalSet{static: static, dynamic: dynamic, staticCount: staticCount, dynamicCount: int64(len(dynamic))}
	result.digest, err = ordinalHybridSetDigest(dictionarySet, static, dynamic)
	return result, err
}

func exceedsOrdinalLimit(used, rootLimits, taskLimits, delta ExposureLimits) bool {
	return (delta.ReleaseFacts > 0 && (delta.ReleaseFacts > rootLimits.ReleaseFacts-used.ReleaseFacts ||
		delta.ReleaseFacts > taskLimits.ReleaseFacts-used.ReleaseFacts)) ||
		(delta.InfluenceFacts > 0 && (delta.InfluenceFacts > rootLimits.InfluenceFacts-used.InfluenceFacts ||
			delta.InfluenceFacts > taskLimits.InfluenceFacts-used.InfluenceFacts)) ||
		(delta.OutcomeFacts > 0 && (delta.OutcomeFacts > rootLimits.OutcomeFacts-used.OutcomeFacts ||
			delta.OutcomeFacts > taskLimits.OutcomeFacts-used.OutcomeFacts))
}

func getOrdinalExposureCharge(ctx context.Context, source rowQueryer, queryID string) (ExposureCharge, error) {
	var charge ExposureCharge
	var status string
	err := source.QueryRowContext(ctx, `
SELECT reservation.query_id,reservation.root_task_id,reservation.profile_version,reservation.status,
 reservation.actual_release_facts,reservation.actual_influence_facts,reservation.actual_outcome_facts,
 reservation.charged_release_facts,reservation.charged_influence_facts,reservation.charged_outcome_facts,
 reservation.observation_sha256,reservation.root_epoch,COALESCE(observation.dictionary_set_digest,''),
 COALESCE(observation.release_set_sha256,''),COALESCE(observation.influence_set_sha256,''),COALESCE(observation.outcome_set_sha256,'')
FROM v4_query_exposure_reservations reservation
LEFT JOIN v4_observations observation ON observation.observation_sha256=reservation.observation_sha256
WHERE reservation.query_id=$1`, queryID).
		Scan(&charge.QueryID, &charge.RootTaskID, &charge.ProfileVersion, &status,
			&charge.ActualReleaseFacts, &charge.ActualInfluenceFacts, &charge.ActualOutcomeFacts,
			&charge.ChargedReleaseFacts, &charge.ChargedInfluenceFacts, &charge.ChargedOutcomeFacts,
			&charge.ObservationSHA256, &charge.RootEpoch, &charge.DictionarySetDigest,
			&charge.ReleaseSetSHA256, &charge.InfluenceSetSHA256, &charge.OutcomeSetSHA256)
	if err != nil {
		return ExposureCharge{}, err
	}
	if status != exposureSettled {
		return ExposureCharge{}, sql.ErrNoRows
	}
	return charge, nil
}

func releaseAnyExposureReservationTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string) error {
	var isV4, isV5 bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS (SELECT 1 FROM v4_query_exposure_reservations WHERE query_id=$1),
 EXISTS (SELECT 1 FROM v5_query_exposure_reservations WHERE query_id=$1)`, queryID).Scan(&isV4, &isV5); err != nil {
		return err
	}
	if isV5 {
		return releaseV5ExposureReservationTx(ctx, tx, now, queryID)
	}
	if !isV4 {
		return releaseExposureReservationTx(ctx, tx, now, queryID)
	}
	var taskID, rootTaskID, status string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,root_task_id,status FROM v4_query_exposure_reservations
WHERE query_id=$1 FOR UPDATE`, queryID).Scan(&taskID, &rootTaskID, &status); err != nil {
		return err
	}
	if status == exposureReleased {
		return nil
	}
	if status != exposureReserved {
		return fmt.Errorf("cannot release %s V4 exposure reservation", status)
	}
	_, err := tx.ExecContext(ctx, `UPDATE v4_query_exposure_reservations SET status='RELEASED',settled_at=$1
WHERE query_id=$2 AND status='RESERVED'`, dbTime(now), queryID)
	if err != nil {
		return err
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: taskID, QueryID: queryID, Actor: "system",
		EventType: "QUERY_ORDINAL_EXPOSURE_RELEASED", Payload: mustJSON(map[string]any{"root_task_id": rootTaskID}),
		OccurredAt: now})
	return err
}

func ordinalCardinalityInt64(value uint64) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("ordinal cardinality exceeds int64")
	}
	return int64(value), nil
}

func ordinalHybridSetDigest(dictionarySet string, static ordinal.BitmapSet, dynamic []OrdinalDynamicFact) (string, error) {
	staticDigest, err := static.Digest()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(hybridSetDigestDomain))
	writeOrdinalDigestPart(hash, dictionarySet)
	writeOrdinalDigestPart(hash, staticDigest)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(dynamic)))
	hash.Write(count[:])
	for _, fact := range dynamic {
		writeOrdinalDigestPart(hash, fact.SHA256)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ordinalObservationDigest(observation normalizedOrdinalObservation) string {
	hash := sha256.New()
	hash.Write([]byte(observationDigestDomain))
	for _, value := range []string{exposure.ProfileV4, observation.dictionarySet,
		observation.release.digest, observation.influence.digest, observation.outcome.digest} {
		writeOrdinalDigestPart(hash, value)
	}
	for _, value := range []int64{observation.release.cardinality(), observation.influence.cardinality(), observation.outcome.cardinality()} {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(value))
		hash.Write(encoded[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type digestWriter interface{ Write([]byte) (int, error) }

func writeOrdinalDigestPart(writer digestWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	writer.Write(length[:])
	writer.Write([]byte(value))
}

// rowQueryer is implemented by *sql.DB and *sql.Tx.
type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// RootReleaseContains reports, for each ref, whether the root family of
// taskID has already settled that Fact into its Release set. The retrieval
// tools run under the V5 profile, whose root head is the V5 one; a V4 head is
// consulted when no V5 head exists. A family that has settled nothing yet
// contains no ref.
func (s *Store) RootReleaseContains(ctx context.Context, taskID string, refs []ordinal.FactRef) ([]bool, error) {
	const op = "root release contains"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	result := make([]bool, len(refs))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer func() { _ = tx.Rollback() }()
	var releaseSet string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(h.release_set_sha256,'')
FROM tasks t JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).Scan(&releaseSet)
	if isNoRows(err) {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(h.release_set_sha256,'')
FROM tasks t JOIN v4_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).Scan(&releaseSet)
	}
	if isNoRows(err) || (err == nil && releaseSet == "") {
		return result, nil
	}
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	set, _, err := loadOrdinalSetTx(ctx, tx, releaseSet)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	for i, ref := range refs {
		result[i] = set.static.Contains(ref)
	}
	return result, nil
}
