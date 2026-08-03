// Package rq5fixture freezes the formal RQ5 daily-publication matrix.
//
// The online execution model is intentionally a single production Gateway
// service slot. A Catalog transition is a sequential stop/start operation in
// that slot. Checking a retained task temporarily stops the new Catalog,
// starts the old Catalog, executes the check, stops it, and restores the new
// Catalog. There is no request router and never more than one live
// gateway.Service at an instant.
package rq5fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	Version             = "taskgate-final-v5-rq5-fixture-v1"
	WorkloadID          = "daily-publication-v5"
	Scale               = "345000"
	RowsPerPublication  = int64(345_000)
	CyclesPerDeployment = 4
	MaximumHOTBytes     = int64(160 << 20)
	DailyCycleGateMS    = float64(300_000)

	BuildMode    = "build_verify_activate"
	RetainedMode = "retained_route"

	TopologyModel      = "single_production_gateway_service_slot_sequential_catalog_restart"
	TopologyDisclosure = "single service slot; at any instant only one production Gateway service; " +
		"Catalog changes use sequential stop-old/start-new restart; retained checks stop new, " +
		"start old, query old, stop old, and restore new; no request router"
)

var Days = [...]string{"day0", "day1", "day2", "day3"}

// Cycle binds each of the four preregistered samples to one independently
// built target publication and its immediately preceding retained publication.
// Day0 uses Day3 as the retained predecessor so every paired retained_route
// sample proves the same old/new invariants; this is an explicit cyclic daily
// fixture, not an assertion that Day3 chronologically preceded the first Day0.
type Cycle struct {
	Index int
	From  string
	To    string
}

func LookupCycle(iteration int) (Cycle, error) {
	if iteration < 1 || iteration > CyclesPerDeployment {
		return Cycle{}, fmt.Errorf("RQ5 iteration %d is outside the four frozen cycles", iteration)
	}
	target := iteration - 1
	from := (target + len(Days) - 1) % len(Days)
	return Cycle{Index: iteration, From: Days[from], To: Days[target]}, nil
}

func IsCell(workloadID, scale, mode string, iteration int) bool {
	if workloadID != WorkloadID || scale != Scale || (mode != BuildMode && mode != RetainedMode) {
		return false
	}
	_, err := LookupCycle(iteration)
	return err == nil
}

// FixtureSHA256 is a stable source-controlled identity for the matrix and its
// service-slot semantics. Publication artifact digests are separately bound in
// live evidence because they are produced by the measured build.
func FixtureSHA256() string {
	value := struct {
		Version             string   `json:"version"`
		WorkloadID          string   `json:"workload_id"`
		Scale               string   `json:"scale"`
		RowsPerPublication  int64    `json:"rows_per_publication"`
		CyclesPerDeployment int      `json:"cycles_per_deployment"`
		MaximumHOTBytes     int64    `json:"maximum_hot_bytes"`
		DailyCycleGateMS    float64  `json:"daily_cycle_gate_ms"`
		Modes               []string `json:"modes"`
		Days                []string `json:"days"`
		TopologyModel       string   `json:"topology_model"`
		TopologyDisclosure  string   `json:"topology_disclosure"`
	}{
		Version: Version, WorkloadID: WorkloadID, Scale: Scale,
		RowsPerPublication: RowsPerPublication, CyclesPerDeployment: CyclesPerDeployment,
		MaximumHOTBytes: MaximumHOTBytes, DailyCycleGateMS: DailyCycleGateMS,
		Modes: []string{BuildMode, RetainedMode}, Days: Days[:],
		TopologyModel: TopologyModel, TopologyDisclosure: TopologyDisclosure,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func Validate() error {
	if len(Days) != CyclesPerDeployment || RowsPerPublication != 345_000 || MaximumHOTBytes != 160<<20 ||
		DailyCycleGateMS != 300_000 || FixtureSHA256() == "" {
		return errors.New("RQ5 fixture constants changed")
	}
	seenTargets := map[string]bool{}
	for index := 1; index <= CyclesPerDeployment; index++ {
		cycle, err := LookupCycle(index)
		if err != nil || cycle.From == cycle.To || seenTargets[cycle.To] {
			return errors.New("RQ5 cyclic transition matrix is invalid")
		}
		seenTargets[cycle.To] = true
	}
	return nil
}
