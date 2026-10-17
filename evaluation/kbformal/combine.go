package kbformal

import (
	"fmt"
	"strconv"
	"strings"
)

// Combining the per-deployment driver results of a round into one report per
// experiment, in the shape of the registered pilot reports (evaluation/kb-pilot),
// so the paper generator reads formal and pilot evidence the same way. Each
// summary is the pilot runner's computation (internal/gateway/kb_experiment_*_test.go)
// ported line for line and accumulated in the same order, so floating-point
// results are identical; combine_test.go recomputes the registered pilot
// reports' summaries from their own per-deployment details.

// CombineMeta is what every combined report states about its provenance.
type CombineMeta struct {
	CampaignClass       string
	PublicationEligible bool
	GeneratedAt         string
	Epoch               string
}

func (meta CombineMeta) header(version string) map[string]any {
	return map[string]any{"version": version, "campaign_class": meta.CampaignClass, "publication_eligible": meta.PublicationEligible,
		"generated_at": meta.GeneratedAt, "publication_epoch": meta.Epoch}
}

// SummarizeEKB1 is the pilot E-KB1 summary: per arm/strategy the runs and the
// mean, maximum and minimum leak fraction and the latest first refusal, and
// the legitimate-task totals.
func SummarizeEKB1(details []EKB1Deployment) (map[string]any, map[string]any) {
	type agg struct {
		maxLeak, minLeak, sumLeak float64
		n                         int
		firstRefusalMax           int
	}
	summary := map[string]*agg{}
	for _, report := range details {
		for _, trajectory := range report.Trajectories {
			key := trajectory.Arm + "/" + trajectory.Strategy
			a := summary[key]
			if a == nil {
				a = &agg{minLeak: 1}
				summary[key] = a
			}
			a.n++
			a.sumLeak += trajectory.LeakFraction
			if trajectory.LeakFraction > a.maxLeak {
				a.maxLeak = trajectory.LeakFraction
			}
			if trajectory.LeakFraction < a.minLeak {
				a.minLeak = trajectory.LeakFraction
			}
			if trajectory.FirstRefusal > a.firstRefusalMax {
				a.firstRefusalMax = trajectory.FirstRefusal
			}
		}
	}
	out := map[string]any{}
	for key, a := range summary {
		out[key] = map[string]any{"runs": a.n, "leak_mean": a.sumLeak / float64(a.n), "leak_max": a.maxLeak, "leak_min": a.minLeak, "first_refusal_max": a.firstRefusalMax}
	}
	completed, questions, correct, runs := 0, 0, 0, 0
	for _, report := range details {
		for _, legit := range report.Legit {
			completed += legit.Completed
			questions += legit.Questions
			runs++
			if legit.AggregateCorrect {
				correct++
			}
		}
	}
	return out, map[string]any{"questions": questions, "completed": completed, "runs": runs, "aggregate_correct_runs": correct}
}

// CombineEKB1 builds the E-KB1 report (taskgate-kb-ekb1-report-v1).
func CombineEKB1(meta CombineMeta, details []EKB1Deployment, querysetManifest map[string]any, budget Budget, seeds, maxSteps int, seconds float64) map[string]any {
	summary, legitimate := SummarizeEKB1(details)
	report := meta.header("taskgate-kb-ekb1-report-v1")
	report["queryset_manifest"] = querysetManifest
	report["budget"] = map[string]any{"queries": budget.MaxQueries, "release_cells": budget.Release, "influence": budget.Influence, "outcome": budget.Outcome,
		"k_max": 20, "legit_k": 5}
	report["max_steps"], report["deployments"], report["seeds"] = maxSteps, len(details), seeds
	report["summary"], report["legitimate"], report["deployments_detail"], report["seconds"] = summary, legitimate, details, seconds
	return report
}

// SummarizeEKB2a is the pilot E-KB2(a) summary: per request type (as the
// first deployment lists them) the totals and the not-eligible and admitted
// rates.
func SummarizeEKB2a(details []EKB2Deployment) map[string]any {
	summary := map[string]any{}
	if len(details) == 0 {
		return summary
	}
	for kind := range details[0].ByType {
		total := &EKB2Counts{}
		for _, report := range details {
			if c := report.ByType[kind]; c != nil {
				total.Requests += c.Requests
				total.NotEligible += c.NotEligible
				total.Admitted += c.Admitted
				total.OtherErrors += c.OtherErrors
			}
		}
		summary[kind] = map[string]any{"requests": total.Requests, "not_eligible": total.NotEligible, "admitted": total.Admitted, "other_errors": total.OtherErrors,
			"not_eligible_rate": float64(total.NotEligible) / float64(total.Requests), "admitted_rate": float64(total.Admitted) / float64(total.Requests)}
	}
	return summary
}

// CombineEKB2a builds the E-KB2(a) report (taskgate-kb-ekb2-report-v1).
// snapshotSHA256 is the principal attribute snapshot the deployments loaded;
// eligibilityRule is the Catalog's eligibility block.
func CombineEKB2a(meta CombineMeta, details []EKB2Deployment, snapshotSHA256, eligibilityRule string, seconds float64) map[string]any {
	report := meta.header("taskgate-kb-ekb2-report-v1")
	report["snapshot_sha256"], report["subjects"], report["deployments"] = snapshotSHA256, len(EligibilitySubjects()), len(details)
	report["eligibility_rule"], report["summary"], report["deployments_detail"], report["seconds"] = eligibilityRule, SummarizeEKB2a(details), details, seconds
	return report
}

// CombineEKB2b builds the E-KB2(b) report (taskgate-kb-ekb2b-report-v1).
func CombineEKB2b(meta CombineMeta, arms []EKB2bArm, budget Budget, ttlSeconds int) map[string]any {
	report := meta.header("taskgate-kb-ekb2b-report-v1")
	report["budget"] = map[string]any{"queries": budget.MaxQueries, "release_cells": budget.Release, "influence": budget.Influence, "outcome": budget.Outcome, "k_max": 20}
	report["ttl_seconds"], report["arms"] = ttlSeconds, arms
	return report
}

// CombineEKB3 builds the E-KB3 report of one tier (taskgate-kb-ekb3-report-v1):
// the samples of every deployment in deployment order and their summary.
func CombineEKB3(meta CombineMeta, details []EKB3Deployment, startedAt string) (map[string]any, error) {
	if len(details) == 0 {
		return nil, fmt.Errorf("E-KB3 has no deployments")
	}
	var samples []EKB3Sample
	seconds := 0.0
	for _, detail := range details {
		if detail.Tier != details[0].Tier || detail.Chunks != details[0].Chunks || detail.Rounds != details[0].Rounds || detail.Department != details[0].Department {
			return nil, fmt.Errorf("E-KB3 deployment %d ran another tier, corpus, round count or department", detail.Deployment)
		}
		samples = append(samples, detail.Samples...)
		seconds += detail.Seconds
	}
	report := meta.header("taskgate-kb-ekb3-report-v1")
	report["started_at"], report["tier"], report["chunks"], report["rounds"] = startedAt, details[0].Tier, details[0].Chunks, details[0].Rounds
	report["deployments"], report["department"], report["summary"], report["samples"], report["seconds"] = len(details), details[0].Department, SummarizeEKB3(samples), samples, seconds
	return report, nil
}

// SummarizeEKB4 is the pilot E-KB4 summary over every observation without an
// error: hit-set identity and order against the direct arm by k for the text
// and the vector arm, recall@k by query kind, by document type and for the
// exact version, MRR@20 by kind, and the cross-deployment identity of the
// gateway hit lists against the first deployment.
func SummarizeEKB4(details []EKB4Deployment, queries int) map[string]any {
	type bucket struct{ hits, total int }
	identical, ordered := map[int]*bucket{}, map[int]*bucket{}
	vectorIdentical, vectorOrdered := map[int]*bucket{}, map[int]*bucket{}
	recallKind, recallType, recallVersion := map[string]*bucket{}, map[string]*bucket{}, map[int]*bucket{}
	mrr, mrrCount := map[string]float64{}, map[string]int{}
	errors := 0
	count := func(m map[int]*bucket, k int, hit bool) {
		if m[k] == nil {
			m[k] = &bucket{}
		}
		m[k].total++
		if hit {
			m[k].hits++
		}
	}
	countKey := func(m map[string]*bucket, key string, hit bool) {
		if m[key] == nil {
			m[key] = &bucket{}
		}
		m[key].total++
		if hit {
			m[key].hits++
		}
	}
	for _, report := range details {
		for _, observation := range report.Observations {
			if observation.Error != "" {
				errors++
				continue
			}
			count(identical, observation.K, observation.Identical)
			count(ordered, observation.K, observation.SameOrder)
			count(vectorIdentical, observation.K, observation.VectorIdentical)
			count(vectorOrdered, observation.K, observation.VectorSameOrder)
			countKey(recallKind, fmt.Sprintf("%s@%d", observation.Kind, observation.K), observation.RankDoc > 0)
			countKey(recallType, fmt.Sprintf("%s@%d", observation.DocumentType, observation.K), observation.RankDoc > 0)
			count(recallVersion, observation.K, observation.RankVersion > 0)
			if observation.K == 20 {
				mrrCount[observation.Kind]++
				if observation.RankDoc > 0 {
					mrr[observation.Kind] += 1 / float64(observation.RankDoc)
				}
			}
		}
	}
	rate := func(b *bucket) float64 {
		if b == nil || b.total == 0 {
			return 0
		}
		return float64(b.hits) / float64(b.total)
	}
	byK := func(m map[int]*bucket) map[string]float64 {
		out := map[string]float64{}
		for k, b := range m {
			out[strconv.Itoa(k)] = rate(b)
		}
		return out
	}
	byKey := func(m map[string]*bucket) map[string]float64 {
		out := map[string]float64{}
		for key, b := range m {
			out[key] = rate(b)
		}
		return out
	}
	mrrByKind := map[string]float64{}
	for kind, sum := range mrr {
		mrrByKind[kind] = sum / float64(mrrCount[kind])
	}
	crossIdentical, crossTotal := 0, 0
	if len(details) > 1 {
		first := map[string]string{}
		for _, observation := range details[0].Observations {
			first[observation.QueryID+"@"+strconv.Itoa(observation.K)] = strings.Join(observation.Gateway, ",")
		}
		for _, report := range details[1:] {
			for _, observation := range report.Observations {
				crossTotal++
				if first[observation.QueryID+"@"+strconv.Itoa(observation.K)] == strings.Join(observation.Gateway, ",") {
					crossIdentical++
				}
			}
		}
	}
	return map[string]any{"queries": queries, "deployments": len(details), "errors": errors,
		"gateway_vs_direct_identity_by_k": byK(identical), "gateway_vs_direct_same_order_by_k": byK(ordered),
		"vector_arm_identity_by_k": byK(vectorIdentical), "vector_arm_same_order_by_k": byK(vectorOrdered),
		"recall_at_k_by_kind": byKey(recallKind), "recall_at_k_by_document_type": byKey(recallType), "recall_at_k_exact_version": byK(recallVersion),
		"mrr_at_20_by_kind": mrrByKind, "cross_deployment_identical": crossIdentical, "cross_deployment_total": crossTotal}
}

// CombineEKB4 builds the E-KB4 report (taskgate-kb-ekb4-report-v1). The
// summary's seconds is the sum of the deployments' experiment times.
func CombineEKB4(meta CombineMeta, details []EKB4Deployment, querysetManifest map[string]any, queries int, catalogSHA256 string) map[string]any {
	summary := SummarizeEKB4(details, queries)
	seconds := 0.0
	for _, detail := range details {
		seconds += detail.Seconds
	}
	summary["seconds"] = seconds
	report := meta.header("taskgate-kb-ekb4-report-v1")
	report["queryset_manifest"], report["catalog_sha256"], report["ks"] = querysetManifest, catalogSHA256, []int{5, 10, 20}
	report["summary"], report["deployments_detail"] = summary, details
	return report
}

// CombineEKB5 builds the E-KB5 report (taskgate-kb-ekb5-report-v1) from the
// document-scoped and the collection-scoped deployments.
func CombineEKB5(meta CombineMeta, observations []EKB5Observation, documentEpoch, collectionEpoch string, deployments int) map[string]any {
	report := meta.header("taskgate-kb-ekb5-report-v1")
	delete(report, "publication_epoch")
	report["department"], report["classification"], report["field"] = "finance", "internal", "counterparty"
	report["publications"] = map[string]any{"document": documentEpoch, "collection": collectionEpoch}
	report["deployments"], report["observations"] = deployments, observations
	return report
}

// EligibilityBlock returns the eligibility block of a profile Catalog, the
// text the E-KB2(a) report records as its rule.
func EligibilityBlock(catalogText string) string {
	var out []string
	inside := false
	for _, line := range strings.Split(catalogText, "\n") {
		if strings.TrimSpace(line) == "eligibility:" {
			inside = true
			out = append(out, line)
			continue
		}
		if inside {
			if strings.TrimSpace(line) != "" && len(line)-len(strings.TrimLeft(line, " ")) <= len(out[0])-len(strings.TrimLeft(out[0], " ")) {
				break
			}
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}
