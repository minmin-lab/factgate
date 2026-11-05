package kbformal

import (
	"fmt"
	"strings"
)

// ---- Combined reports of the keyword round (E-KB6..E-KB8) ----

// SummarizeEKB6 counts, by k and by keyword class, the observations without an
// error, the member- and order-identical ones, the absent keywords answered by
// an empty set, the anchor keywords whose target was hit, and the gateway hit
// lists identical across deployments to the first deployment's.
func SummarizeEKB6(details []EKB6Deployment) map[string]any {
	type cell struct {
		Observations int `json:"observations"`
		Errors       int `json:"errors"`
		Identical    int `json:"identical"`
		SameOrder    int `json:"same_order"`
		Empty        int `json:"empty"`
		TargetHits   int `json:"target_hits"`
	}
	byK := map[string]*cell{}
	byClass := map[string]*cell{}
	get := func(m map[string]*cell, key string) *cell {
		if m[key] == nil {
			m[key] = &cell{}
		}
		return m[key]
	}
	for _, detail := range details {
		for _, o := range detail.Observations {
			for _, c := range []*cell{get(byK, fmt.Sprintf("k%d", o.K)), get(byClass, fmt.Sprintf("%s/k%d", o.Class, o.K))} {
				c.Observations++
				if o.Error != "" {
					c.Errors++
					continue
				}
				if o.Identical {
					c.Identical++
				}
				if o.SameOrder {
					c.SameOrder++
				}
				if len(o.Gateway) == 0 {
					c.Empty++
				}
				if o.TargetHit {
					c.TargetHits++
				}
			}
		}
	}
	cross, crossTotal := 0, 0
	if len(details) > 0 {
		first := map[string]string{}
		for _, o := range details[0].Observations {
			first[fmt.Sprintf("%s/%d", o.KeywordID, o.K)] = strings.Join(o.Gateway, ",")
		}
		for _, detail := range details[1:] {
			for _, o := range detail.Observations {
				if o.Error != "" {
					continue
				}
				crossTotal++
				if reference, ok := first[fmt.Sprintf("%s/%d", o.KeywordID, o.K)]; ok && reference == strings.Join(o.Gateway, ",") {
					cross++
				}
			}
		}
	}
	return map[string]any{"by_k": byK, "by_class": byClass, "cross_deployment_identical": cross, "cross_deployment_total": crossTotal}
}

// CombineEKB6 builds the E-KB6 report (taskgate-kb-ekb6-report-v1).
func CombineEKB6(meta CombineMeta, details []EKB6Deployment, keywordsetManifest map[string]any, catalogSHA256 string) map[string]any {
	summary := SummarizeEKB6(details)
	seconds := 0.0
	for _, detail := range details {
		seconds += detail.Seconds
	}
	summary["seconds"] = seconds
	report := meta.header("taskgate-kb-ekb6-report-v1")
	report["keywordset_manifest"], report["catalog_sha256"], report["ks"] = keywordsetManifest, catalogSHA256, []int{5, 10, 20}
	report["deployments"], report["summary"], report["deployments_detail"] = len(details), summary, details
	return report
}

// CombineEKB7 builds the E-KB7 report (taskgate-kb-ekb7-report-v1): the
// E-KB1 trajectory summary over strategy S4 and the legitimate-task totals.
func CombineEKB7(meta CombineMeta, details []EKB7Deployment, keywordsetManifest map[string]any, budget Budget, seeds, maxSteps int) map[string]any {
	asEKB1 := make([]EKB1Deployment, 0, len(details))
	seconds := 0.0
	legitimate := map[string]int{}
	for _, detail := range details {
		asEKB1 = append(asEKB1, EKB1Deployment{Deployment: detail.Deployment, Trajectories: detail.Trajectories})
		seconds += detail.Seconds
		for _, legit := range detail.Legit {
			legitimate["runs"]++
			legitimate["questions"] += legit.Questions
			legitimate["completed"] += legit.Completed
			legitimate["visible_targets"] += legit.Visible
			legitimate["completed_visible"] += legit.CompletedVisible
			legitimate["refusals"] += legit.Refusals
		}
	}
	summary, _ := SummarizeEKB1(asEKB1)
	report := meta.header("taskgate-kb-ekb7-report-v1")
	report["keywordset_manifest"] = keywordsetManifest
	report["budget"] = map[string]any{"queries": budget.MaxQueries, "release_cells": budget.Release, "influence": budget.Influence, "outcome": budget.Outcome,
		"k_max": 20, "legit_k": 5}
	report["max_steps"], report["deployments"], report["seeds"] = maxSteps, len(details), seeds
	report["summary"], report["legitimate"], report["deployments_detail"], report["seconds"] = summary, legitimate, details, seconds
	return report
}

// CombineEKB8 builds the E-KB8 report (taskgate-kb-ekb8-report-v1) with the
// E-KB3 summary over its keyword samples.
func CombineEKB8(meta CombineMeta, details []EKB3Deployment, startedAt string) (map[string]any, error) {
	report, err := CombineEKB3(meta, details, startedAt)
	if err != nil {
		return nil, err
	}
	report["version"] = "taskgate-kb-ekb8-report-v1"
	return report, nil
}
