package kbformal

import (
	"fmt"
	"sort"
)

// Round is one formal round of the knowledge-product evaluation: the profiles
// it deploys and the dataset binding the author approves for it. The first
// round (kb, 2026-10-17, sealed) ran E-KB1..E-KB5; the keyword round runs
// E-KB6..E-KB8 (docs/kb_experiment_design.md, 关键词检索实验).
type Round struct {
	Name           string
	BindingVersion string
	BindingPath    string
	// RegistrationPath is where kb-formal-register writes the round's
	// formal evidence registration.
	RegistrationPath string
	Profiles         []string
	// KeywordSet binds the frozen keyword set of the keyword experiments.
	KeywordSet bool
}

// KeywordsetDir is the frozen keyword set of the keyword round.
const KeywordsetDir = "evaluation/kb-formal/keywordset-v1"

// Rounds are the defined rounds by name.
var Rounds = map[string]Round{
	"kb": {Name: "kb", BindingVersion: BindingVersion, BindingPath: BindingPath,
		RegistrationPath: "evaluation/kb-formal/kb-publication-evidence-v1.json",
		Profiles:         []string{"kb-v2-pilot", "kb-v2-large", "kb-20k-large", "kb-v2c-pilot"}},
	"keyword": {Name: "keyword", BindingVersion: "taskgate-kb-dataset-binding-v2", BindingPath: "evaluation/kb-formal/kb-dataset-binding-v2.json",
		RegistrationPath: "evaluation/kb-formal/kb-keyword-evidence-v1.json",
		Profiles:         []string{"kb-v2kw-pilot", "kb-v2kw-large", "kb-20kkw-large"}, KeywordSet: true},
}

// LookupRound returns the named round.
func LookupRound(name string) (Round, error) {
	round, ok := Rounds[name]
	if !ok {
		names := make([]string, 0, len(Rounds))
		for candidate := range Rounds {
			names = append(names, candidate)
		}
		sort.Strings(names)
		return Round{}, fmt.Errorf("unknown round %q (defined: %v)", name, names)
	}
	return round, nil
}

// Includes reports whether the round deploys the named profile.
func (round Round) Includes(profile string) bool {
	for _, name := range round.Profiles {
		if name == profile {
			return true
		}
	}
	return false
}
