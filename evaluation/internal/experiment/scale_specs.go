package experiment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type DependencyScaleSpec struct {
	CandidateFacts int64
	OverlapPercent int
	OverlapFacts   int64
}

type OutcomeMerkleScaleSpec struct {
	RootFacts      int64
	CandidateFacts int64
	OverlapPercent int
	OverlapFacts   int64
}

type ArtifactScaleSpec struct {
	Rows    int64
	Columns int
}

func ParseDependencyScale(value string) (DependencyScaleSpec, error) {
	parts := strings.Split(value, "-overlap-")
	if len(parts) != 2 {
		return DependencyScaleSpec{}, errors.New("dependency scale is not frozen")
	}
	facts, ok := map[string]int64{"10k": 10_000, "100k": 100_000, "1035000": 1_035_000}[parts[0]]
	percent, err := parseFrozenOverlap(parts[1])
	if !ok || err != nil {
		return DependencyScaleSpec{}, errors.New("dependency scale is not frozen")
	}
	return DependencyScaleSpec{CandidateFacts: facts, OverlapPercent: percent,
		OverlapFacts: facts * int64(percent) / 100}, nil
}

func ParseOutcomeMerkleScale(value string) (OutcomeMerkleScaleSpec, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "x") || !strings.HasPrefix(parts[2], "o") {
		return OutcomeMerkleScaleSpec{}, errors.New("Outcome-Merkle scale is not frozen")
	}
	root, rootOK := map[string]int64{"10k": 10_000, "100k": 100_000, "1m": 1_000_000}[parts[0]]
	candidate, candidateOK := map[string]int64{"x1": 1, "x100": 100, "x10k": 10_000}[parts[1]]
	percent, err := parseFrozenOverlap(strings.TrimPrefix(parts[2], "o"))
	if !rootOK || !candidateOK || err != nil {
		return OutcomeMerkleScaleSpec{}, errors.New("Outcome-Merkle scale is not frozen")
	}
	// x1 cannot represent 50% or 90% exactly. The preregistered label is
	// therefore resolved by the nearest integer, with exact halves rounded up.
	// This rule is retained in every raw sample and enforced by the finalizer.
	overlap := (candidate*int64(percent) + 50) / 100
	return OutcomeMerkleScaleSpec{RootFacts: root, CandidateFacts: candidate,
		OverlapPercent: percent, OverlapFacts: overlap}, nil
}

func ParseArtifactScale(value string) (ArtifactScaleSpec, error) {
	parts := strings.Split(value, "x")
	if len(parts) != 2 {
		return ArtifactScaleSpec{}, errors.New("artifact scale is not frozen")
	}
	rows, rowsOK := map[string]int64{"100": 100, "10k-": 10_000, "100k-": 100_000}[parts[0]]
	columns, err := strconv.Atoi(parts[1])
	if !rowsOK || err != nil || (columns != 4 && columns != 16) {
		return ArtifactScaleSpec{}, errors.New("artifact scale is not frozen")
	}
	return ArtifactScaleSpec{Rows: rows, Columns: columns}, nil
}

func ParseExtremeScale(value string) (int64, error) {
	if facts, ok := map[string]int64{"10m": 10_000_000, "100m": 100_000_000}[value]; ok {
		return facts, nil
	}
	return 0, fmt.Errorf("extreme scale %q is not frozen", value)
}

func parseFrozenOverlap(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || (parsed != 0 && parsed != 50 && parsed != 90 && parsed != 100) {
		return 0, errors.New("overlap is not frozen")
	}
	return parsed, nil
}
