package finalv5profile

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ActivationInvocation is the single evaluation-side description of a call to
// final-v5-profile-activate. Callers may choose go run or a prebuilt binary,
// but they must not reconstruct the activator's flag contract independently.
type ActivationInvocation struct {
	Root                    string
	ComposeProject          string
	ComposeFiles            string
	DeploymentID            string
	ProfileID               string
	RegistryPath            string
	GatewayURL              string
	AdminTokenEnv           string
	PreviousProfileID       string
	Sequence                int
	EvidenceOut             string
	OutsideProducts         string
	ProfileArtifactDir      string
	ProfileArtifactManifest string
	BusinessDSNEnv          string
	SchemaAttestations      string
	ProbeTokenEnv           string
	ReadyTimeout            time.Duration
	DatasetBinding          string
}

// Arguments returns the activator flags after checking the shared live-switch
// prerequisites. It deliberately contains no docker or task-lifecycle logic:
// those remain owned by final-v5-profile-activate and the calling workload.
func (invocation ActivationInvocation) Arguments() ([]string, error) {
	for label, value := range map[string]string{
		"root": invocation.Root, "compose project": invocation.ComposeProject,
		"compose files": invocation.ComposeFiles, "deployment ID": invocation.DeploymentID,
		"profile ID": invocation.ProfileID, "registry path": invocation.RegistryPath,
		"gateway URL": invocation.GatewayURL, "admin token environment": invocation.AdminTokenEnv,
		"evidence output": invocation.EvidenceOut, "outside Products": invocation.OutsideProducts,
		"profile artifact directory": invocation.ProfileArtifactDir,
		"profile artifact manifest":  invocation.ProfileArtifactManifest,
		"Business DSN environment":   invocation.BusinessDSNEnv,
		"schema attestations":        invocation.SchemaAttestations,
		"probe token environment":    invocation.ProbeTokenEnv,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("activation invocation %s is empty", label)
		}
	}
	if invocation.Sequence < 1 {
		return nil, errors.New("activation invocation sequence must be positive")
	}
	if invocation.ReadyTimeout <= 0 {
		return nil, errors.New("activation invocation readiness timeout must be positive")
	}
	arguments := []string{"-root", invocation.Root, "-compose-project", invocation.ComposeProject,
		"-compose-files", invocation.ComposeFiles, "-deployment-id", invocation.DeploymentID,
		"-profile-id", invocation.ProfileID, "-registry", invocation.RegistryPath,
		"-gateway-url", invocation.GatewayURL, "-admin-token-env", invocation.AdminTokenEnv,
		"-activation-sequence", fmt.Sprint(invocation.Sequence), "-evidence-out", invocation.EvidenceOut,
		"-outside-products", invocation.OutsideProducts,
		"-profile-artifact-dir", invocation.ProfileArtifactDir,
		"-profile-artifact-manifest", invocation.ProfileArtifactManifest,
		"-business-dsn-env", invocation.BusinessDSNEnv,
		"-schema-attestations", invocation.SchemaAttestations,
		"-probe-token-env", invocation.ProbeTokenEnv,
		"-ready-timeout", invocation.ReadyTimeout.String()}
	if invocation.PreviousProfileID != "" {
		arguments = append(arguments, "-previous-profile-id", invocation.PreviousProfileID)
	}
	if invocation.DatasetBinding != "" {
		arguments = append(arguments, "-dataset-binding", invocation.DatasetBinding)
	}
	return arguments, nil
}
