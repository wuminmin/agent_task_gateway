package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var operation experiment.AdapterOperation
		if experiment.StrictJSON(scanner.Bytes(), &operation) != nil || emit(encoder, operation) != nil {
			os.Exit(1)
		}
	}
	if scanner.Err() != nil {
		os.Exit(1)
	}
}

func emit(encoder *json.Encoder, operation experiment.AdapterOperation) error {
	resultHash, _ := experiment.CanonicalResultHash([][]any{{operation.WorkloadID, operation.Scale, int64(operation.Iteration)}})
	rootIdentity := strings.Join([]string{operation.DeploymentID, operation.WorkloadID, operation.Scale, strconv.Itoa(operation.ProcessReplicate), strconv.Itoa(operation.Iteration), operation.RootGroupID}, "\x00")
	rootBytes := sha256.Sum256([]byte(rootIdentity))
	system := "taskgate"
	if operation.Mode == "direct" {
		system = "postgresql"
	}
	sample := experiment.Sample{
		SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
		ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
		Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, OrderPosition: operation.OrderPosition,
		RandomSeed: operation.RandomSeed, System: system, Mode: operation.Mode, WorkloadID: operation.WorkloadID,
		Scale: operation.Scale, ClientAvailableMS: .07, ClientFullDrainMS: .08,
		PipelineMS:   map[string]float64{"prepare": .01, "execute_and_derive": .02, "artifact_stage": .01, "control_settlement": .01, "artifact_publication": .01, "response_finalize": .01, "server_total": .08},
		DiagnosticMS: map[string]float64{}, RowCount: 1, ColumnCount: 3, ResultSHA256: resultHash,
		RootTaskIDHash: hex.EncodeToString(rootBytes[:]), ReceiptVersion: "8",
		SemanticReplay: operation.Mode == "semantic_replay", IdempotentReplay: operation.Mode == "idempotent_replay",
		Status: "pass", PublicationEligible: false,
	}
	return encoder.Encode(sample)
}
