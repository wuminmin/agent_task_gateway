//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package experiment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

// The whole deployment side, assembled: the contract resolver, the profile
// resolver and the retained qualification together produce a pre-registered
// classification.
//
// This is the test that says the five resolvers are wired to material that
// works. OpenObserverWindowV3 prepares the operation from the resolved plan and
// grant, against the resolved Catalog and the published artifacts, derives the
// control plan from the resolved footprint, and builds the classifier manifest
// -- so a digest coming back means every one of those steps ran on real
// material rather than on a value a test wrote down.
func TestTheDeploymentResolversPreRegisterAClassification(t *testing.T) {
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	directory := retainedQualificationDirectory(t)
	finalizer, err := openRuntimeFinalizerV3(verifier,
		deploymentContractsForTest(t),
		deploymentProfilesForTest(t),
		retainedQualificationV3{documentPath: filepath.Join(directory, "attestation-footprint-v2.json")},
		retainedPostgreSQLIdentityV3{documentPath: filepath.Join(directory, "postgresql-identity.json")},
		// The Control Store is the one collaborator this test cannot run: it needs
		// a settled request in a running deployment. Pre-registration reaches none
		// of it -- it happens before the request exists.
		stubControl{state: requestSettlementStateV3{WroteExecutionBindingRow: true}}, stubScaleSets{})
	if err != nil {
		t.Fatalf("open the finalizer: %v", err)
	}
	committed, err := finalizer.OpenObserverWindowV3(context.Background(), FrozenContractSelectorV3{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
		WorkloadID:   finalv5contracts.ArtifactWorkloadID,
		Scale:        "100x4", Mode: "novel",
	}, ObserverAttemptV3{TaskID: "task-deployment-preregistration", RequestID: "request-deployment-preregistration"})
	if err != nil {
		t.Fatalf("pre-register the classification from deployment material: %v", err)
	}
	if !validSHA256(committed.ClassifierManifestSHA256) || !validSHA256(committed.ClassifierBindingSHA256) {
		t.Fatalf("the pre-registered classification is %+v", committed)
	}
	if err := committed.Operation.Validate(); err != nil {
		t.Fatalf("the pre-registered operation identity: %v", err)
	}
	if err := committed.Plan.Validate(); err != nil {
		t.Fatalf("the pre-registered control plan: %v", err)
	}

	// The finalizer also issues the random window identity needed to make the
	// deployment material a runnable observer invocation for this one attempt.
	if err := (ObserverInvocationV3{Phase: "before", ObserverWindowID: committed.ObserverWindowID,
		ClassifierManifestSHA256: committed.ClassifierManifestSHA256}).Validate(); err != nil {
		t.Fatalf("the deployment material does not produce a runnable observer invocation: %v", err)
	}
}

// The retained qualification is accepted only where its own recorded digest
// agrees with the footprint it carries, and where that footprint qualifies the
// ExpectedSchema and the server this run uses.
func TestTheQualificationResolverSelfChecksAndBinds(t *testing.T) {
	directory := retainedQualificationDirectory(t)
	qualification := retainedQualificationV3{
		documentPath: filepath.Join(directory, "attestation-footprint-v2.json"),
	}
	identity := retainedPostgreSQLIdentityV3{
		documentPath: filepath.Join(directory, "postgresql-identity.json"),
	}
	postgres, err := identity.ReadPostgreSQLIdentity(context.Background())
	if err != nil {
		t.Fatalf("read the retained PostgreSQL identity: %v", err)
	}
	footprint, err := qualification.Resolve(resultHeavyCatalogPath(t), postgres)
	if err != nil {
		t.Fatalf("resolve the qualified footprint: %v", err)
	}
	if len(footprint.InternalKeys()) == 0 {
		t.Fatal("the qualified footprint measured no PostgreSQL-internal statement")
	}

	// A different server is refused. The footprint scales with the ExpectedSchema
	// and is a property of one server build, so a qualification carried across
	// either binding would be asserting a measurement that was never made.
	if _, err := qualification.Resolve(resultHeavyCatalogPath(t), testRuntimeIdentity()); err == nil {
		t.Fatal("a footprint qualified elsewhere was accepted for this deployment")
	}

	// And a document whose footprint no longer digests to what it records is
	// refused, which is what makes the retained file self-checking rather than
	// merely present.
	payload, err := os.ReadFile(qualification.documentPath)
	if err != nil {
		t.Fatalf("read the retained qualification: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode the retained qualification: %v", err)
	}
	document["footprint_sha256"] = strings.Repeat("b", 64)
	edited, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encode the retained qualification: %v", err)
	}
	path := filepath.Join(t.TempDir(), "attestation-footprint-v2.json")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write the edited qualification: %v", err)
	}
	if _, err := (retainedQualificationV3{documentPath: path}).Resolve(
		resultHeavyCatalogPath(t), postgres); err == nil {
		t.Fatal("a qualification that disagrees with its own recorded digest was accepted")
	}
}
