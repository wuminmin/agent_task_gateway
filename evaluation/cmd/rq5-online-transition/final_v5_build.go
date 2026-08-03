package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

func loadFinalV5BuildEvidence(directory string,
	request finalV5RQ5DriverRequest) (experiment.RQ5BuildEvidence, error) {
	reports := make(map[string]finalV5PhaseReport, 3)
	commands := make(map[string]finalV5OfflineCommandReport, 3)
	for _, phase := range []string{"build", "strict_verify", "activation"} {
		var report finalV5PhaseReport
		if err := decodeJSONFileStrict(filepath.Join(directory, phase+".json"), &report); err != nil {
			return experiment.RQ5BuildEvidence{}, fmt.Errorf("decode %s phase report: %w", phase, err)
		}
		if report.SchemaVersion != "taskgate-daily-publication-phase-v1" || report.Status != "pass" ||
			report.Phase != phase || report.Day != request.ToDay || report.Sample != request.CycleIndex ||
			report.Executable != "v4-offline" || report.ExitCode != 0 || report.PeakRSSBytes == nil ||
			*report.PeakRSSBytes == 0 || report.WallMS <= 0 || !sha256Regexp.MatchString(report.ExecutableSHA256) ||
			!sha256Regexp.MatchString(report.ArgvSHA256) || !sha256Regexp.MatchString(report.StdoutSHA256) ||
			report.StderrBytes != 0 || len(report.CommandReport) == 0 {
			return experiment.RQ5BuildEvidence{}, fmt.Errorf("%s phase report is not a successful measured v4-offline process", phase)
		}
		var command finalV5OfflineCommandReport
		if err := json.Unmarshal(report.CommandReport, &command); err != nil || command.SchemaVersion != 1 ||
			len(command.Publications) != 1 || command.Publications[0].PublicationName !=
			fmt.Sprintf("daily-lineitem-%s-r%d", request.ToDay, rq5fixture.RowsPerPublication) ||
			command.Publications[0].RowCount != uint64(rq5fixture.RowsPerPublication) {
			return experiment.RQ5BuildEvidence{}, fmt.Errorf("%s command report is not bound to the target publication", phase)
		}
		wantMode := map[string]string{"build": "build", "strict_verify": "verify", "activation": "activate"}[phase]
		if command.Mode != wantMode {
			return experiment.RQ5BuildEvidence{}, fmt.Errorf("%s command mode = %q", phase, command.Mode)
		}
		reports[phase], commands[phase] = report, command
	}
	buildMeasurement := commands["build"].Publications[0]
	for _, phase := range []string{"strict_verify", "activation"} {
		measurement := commands[phase].Publications[0]
		if measurement != buildMeasurement || commands[phase].TotalArtifactBytes != commands["build"].TotalArtifactBytes ||
			commands[phase].HotArtifactBytes != commands["build"].HotArtifactBytes {
			return experiment.RQ5BuildEvidence{}, errors.New("build, read-only strict verification, and activation measured different target artifacts")
		}
	}
	receipt := commands["strict_verify"].VerificationReceiptSHA256
	if !sha256Regexp.MatchString(receipt) || commands["activation"].VerificationReceiptSHA256 != receipt {
		return experiment.RQ5BuildEvidence{}, errors.New("activation is not bound to the current strict-verification receipt")
	}
	phaseEvidence := func(name string) experiment.RQ5PhaseEvidence {
		report := reports[name]
		commandBytes, _ := json.Marshal(report.CommandReport)
		return experiment.RQ5PhaseEvidence{Phase: name, Status: report.Status, WallMS: report.WallMS,
			PeakRSSBytes: int64(*report.PeakRSSBytes), ExecutableSHA256: report.ExecutableSHA256,
			ArgvSHA256: report.ArgvSHA256, StdoutSHA256: report.StdoutSHA256,
			CommandReportSHA256: finalV5Hash(commandBytes)}
	}
	result := experiment.RQ5BuildEvidence{Day: request.ToDay, RowCount: rq5fixture.RowsPerPublication,
		ArtifactBytes: buildMeasurement.ArtifactBytes, HOTArtifactBytes: buildMeasurement.HotArtifactBytes,
		PublicationManifestSHA256: buildMeasurement.ManifestDigest,
		DictionarySHA256:          buildMeasurement.DictionaryDigest, VerificationReceiptSHA256: receipt,
		Build: phaseEvidence("build"), StrictVerify: phaseEvidence("strict_verify"),
		Activation: phaseEvidence("activation")}
	result.CycleWallMS = result.Build.WallMS + result.StrictVerify.WallMS + result.Activation.WallMS
	if result.CycleWallMS > rq5fixture.DailyCycleGateMS {
		return result, errors.New("current target build+strict-verify+activation exceeded five minutes")
	}
	return result, nil
}

func loadFinalV5PublicationSet(ctx context.Context, inputDirectory string, artifactRoots,
	businessDSNs map[string]string) (map[string]finalV5PublicationRuntimeBinding,
	[]experiment.RQ5PublicationEvidence, error) {
	bindings := make(map[string]finalV5PublicationRuntimeBinding, len(days))
	result := make([]experiment.RQ5PublicationEvidence, 0, len(days))
	for index, day := range days {
		publication, err := loadVerifiedPublication(day, inputDirectory, artifactRoots[day])
		if err != nil {
			return nil, nil, fmt.Errorf("load %s publication set member: %w", day, err)
		}
		logicalCatalog, _, err := catalogArtifact(publication)
		if err != nil {
			return nil, nil, err
		}
		connector, attestation, err := openPublicationConnector(ctx, businessDSNs[day], publication.Input,
			publication.Input.Snapshot.SchemaDigest, businessDatabase+"_"+day)
		if err != nil {
			return nil, nil, err
		}
		deployment := &retainedDeployment{Day: day, Catalog: logicalCatalog, Publication: publication, Connector: connector}
		oracle, oracleErr := directSnapshotOracle(ctx, deployment)
		connector.Close()
		if oracleErr != nil || attestation.SchemaDigest != publication.Bundle.DictionaryManifest.SchemaDigest {
			return nil, nil, fmt.Errorf("%s direct frozen-publication oracle or schema binding failed: %w", day, oracleErr)
		}
		bundlePath := filepath.Join(publication.Directory, publication.Input.PublicationName+".bundle.json")
		bundleSHA, err := fileSHA256(bundlePath)
		if err != nil {
			return nil, nil, err
		}
		inputSHA, err := fileSHA256(filepath.Join(inputDirectory, day+".json"))
		if err != nil {
			return nil, nil, err
		}
		entries, err := os.ReadDir(publication.Directory)
		if err != nil {
			return nil, nil, err
		}
		var artifactBytes int64
		for _, entry := range entries {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return nil, nil, errors.New("publication artifact directory changed during measurement")
			}
			artifactBytes += info.Size()
		}
		evidence := experiment.RQ5PublicationEvidence{Index: index, Day: day,
			PublicationName: publication.Input.PublicationName, RowCount: int64(publication.Bundle.RowCount),
			ApprovedInputSHA256: inputSHA, CatalogSHA256: logicalCatalog.SHA256,
			BundleManifestSHA256: bundleSHA, PublicationManifestSHA256: publication.Bundle.ManifestDigest,
			DictionarySHA256:  publication.Bundle.DictionaryManifest.DictionaryDigest,
			SidecarSHA256:     publication.Bundle.DictionaryManifest.SidecarDigest,
			SchemaSHA256:      publication.Bundle.DictionaryManifest.SchemaDigest,
			HOTArtifactSHA256: publication.Bundle.Hot.SHA256, ColdArtifactSHA256: publication.Bundle.Cold.SHA256,
			SidecarArtifactSHA256: publication.Bundle.Sidecar.SHA256, DirectResultSHA256: oracle.ResultSHA256,
			ArtifactBytes: artifactBytes, HOTArtifactBytes: publication.Bundle.Hot.Bytes}
		bindings[day] = finalV5PublicationRuntimeBinding{publication: publication,
			catalogSHA: logicalCatalog.SHA256, oracle: oracle, evidence: evidence}
		result = append(result, evidence)
		// The publication set records only descriptors and digests. Runtime Start
		// reloads exactly one HOT dictionary into the service slot.
		bindingsValue := bindings[day]
		bindingsValue.publication.Index = nil
		bindings[day] = bindingsValue
	}
	return bindings, result, nil
}
