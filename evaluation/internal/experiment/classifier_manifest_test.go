package experiment

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

func testTargets(t *testing.T) []ClassifierEntry {
	t.Helper()
	visible, err := TargetEntry(V3TargetedVisible,
		`SELECT row_id, category FROM reporting.final_v5_result_heavy WHERE row_id <= $1 ORDER BY row_id`,
		"artifact/result-heavy/100x4#visible")
	if err != nil {
		t.Fatalf("visible target: %v", err)
	}
	companion, err := TargetEntry(V3TargetedCompanion,
		`SELECT ordinal FROM taskgate_ordinal.final_v5_result_heavy_v1 WHERE row_id <= $1`,
		"artifact/result-heavy/100x4#companion")
	if err != nil {
		t.Fatalf("companion target: %v", err)
	}
	return []ClassifierEntry{visible, companion}
}

func testManifest(t *testing.T) ClassifierManifest {
	t.Helper()
	manifest, err := BuildClassifierManifest(qualifiedFootprint(t), testTargets(t))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return manifest
}

func TestClassifierManifestIsValidAndDeterministic(t *testing.T) {
	manifest := testManifest(t)
	digest, err := manifest.SHA256()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(digest) != 64 {
		t.Fatalf("manifest digest is not SHA-256: %q", digest)
	}
	for i := 0; i < 8; i++ {
		again, err := BuildClassifierManifest(qualifiedFootprint(t), testTargets(t))
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		other, err := again.SHA256()
		if err != nil {
			t.Fatalf("rebuild digest: %v", err)
		}
		if other != digest {
			t.Fatalf("manifest digest is not deterministic: %s then %s", digest, other)
		}
	}
}

// The manifest must carry identities, never statements.
func TestClassifierManifestContainsNoSQL(t *testing.T) {
	manifest := testManifest(t)
	canonical, err := manifest.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	_ = canonical
	for _, entry := range manifest.Entries {
		for _, field := range []string{entry.StrictASTSHA256, entry.SourceSHA256, entry.ContractIdentity} {
			for _, token := range []string{"SELECT", "select ", "set_config", "pg_get_viewdef", "reporting."} {
				if strings.Contains(field, token) {
					t.Fatalf("manifest entry for %s leaks SQL through %q", entry.Class, field)
				}
			}
		}
	}
}

// Control entries must be generated from the exact Connector constants. A
// production edit must change the manifest, which is what pins the constants the
// strict AST digest deliberately ignores.
func TestControlEntriesArePinnedToConnectorSourceBytes(t *testing.T) {
	manifest := testManifest(t)
	byClass := map[GatewayStatementClassV3]ClassifierEntry{}
	for _, entry := range manifest.Entries {
		byClass[entry.Class] = entry
	}
	for class, sql := range map[GatewayStatementClassV3]string{
		V3SafetySessionPin:      dataconnector.SafetySessionPinSQL,
		V3RepresentationPin:     dataconnector.RepresentationPinSQL,
		V3StatementTimeoutPin:   dataconnector.StatementTimeoutPinSQL,
		V3DatasourceIdentity:    dataconnector.DatasourceIdentitySQL,
		V3ViewColumnAttestation: dataconnector.ViewColumnAttestationSQL,
		V3ViewDefinitionAttest:  dataconnector.ViewDefinitionAttestationSQL,
	} {
		entry := byClass[class]
		if entry.SourceKind != SourceConnectorConstant {
			t.Errorf("class %s is not pinned to a Connector constant", class)
		}
		if entry.SourceSHA256 != sourceDigest(sql) {
			t.Errorf("class %s source digest does not match the production constant", class)
		}
		want, err := StrictASTDigest(sql)
		if err != nil {
			t.Fatal(err)
		}
		if entry.StrictASTSHA256 != want {
			t.Errorf("class %s strict AST digest does not match the production constant", class)
		}
	}
}

// The nested lookup is only itself when PostgreSQL recorded it as nested.
func TestNestedRewriteEntryRequiresNonTopLevel(t *testing.T) {
	manifest := testManifest(t)
	var nested ClassifierEntry
	for _, entry := range manifest.Entries {
		if entry.Class == V3PostgreSQLInternalAttestation {
			nested = entry
		}
	}
	if nested.RequiredTopLevel {
		t.Fatal("the nested rewrite lookup is registered as a top-level statement")
	}
	// The same shape observed at top level must not classify as the nested
	// lookup; toplevel is part of the key precisely for this.
	if class := manifest.Classify(nested.StrictASTSHA256, true); class != V3Unexpected {
		t.Fatalf("a top-level statement with the nested shape classified as %s", class)
	}
	if class := manifest.Classify(nested.StrictASTSHA256, false); class != V3PostgreSQLInternalAttestation {
		t.Fatalf("the nested lookup classified as %s", class)
	}
}

// Every control statement in the closed world must classify to itself, and each
// must be distinct. BEGIN and COMMIT are top-level.
func TestManifestClassifiesEveryControlToItself(t *testing.T) {
	manifest := testManifest(t)
	for class, sql := range map[GatewayStatementClassV3]string{
		V3SafetySessionPin:      dataconnector.SafetySessionPinSQL,
		V3RepresentationPin:     dataconnector.RepresentationPinSQL,
		V3StatementTimeoutPin:   dataconnector.StatementTimeoutPinSQL,
		V3DatasourceIdentity:    dataconnector.DatasourceIdentitySQL,
		V3ViewColumnAttestation: dataconnector.ViewColumnAttestationSQL,
		V3ViewDefinitionAttest:  dataconnector.ViewDefinitionAttestationSQL,
		V3TransactionBegin:      runtimeBeginTemplate,
		V3TransactionCommit:     runtimeCommitTemplate,
	} {
		digest, err := StrictASTDigest(sql)
		if err != nil {
			t.Fatalf("%s: %v", class, err)
		}
		if got := manifest.Classify(digest, true); got != class {
			t.Errorf("class %s classified as %s", class, got)
		}
	}
}

// A structurally different decoy is unexpected outright.
func TestStructurallyDifferentDecoyIsUnexpected(t *testing.T) {
	manifest := testManifest(t)
	for name, sql := range map[string]string{
		"an unrelated read":                    `SELECT * FROM reporting.salary`,
		"a pg_get_viewdef token in other SQL":  `SELECT 1 WHERE pg_get_viewdef($1) IS NOT NULL`,
		"a pg_attribute token in other SQL":    `SELECT count(*) FROM pg_attribute`,
		"the target relation plus another one": `SELECT a FROM reporting.final_v5_result_heavy, reporting.salary`,
		"a prefix collision on the relation":   `SELECT a FROM reporting.final_v5_result_heavy_extra`,
	} {
		t.Run(name, func(t *testing.T) {
			digest, err := StrictASTDigest(sql)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if class := manifest.Classify(digest, true); class != V3Unexpected {
				t.Fatalf("%s classified as %s, want unexpected", name, class)
			}
		})
	}
}

// The additive same-shape decoys, with the collision boundary as measured
// rather than as assumed. See docs/final_v5_observer_v3_classifier_design.md
// case 2a: these are caught by over-count, not by classification.
//
// The boundary is finer than "same arity". Because pg_stat_statements numbers
// the constants it erases around any real bind parameter, the timeout pin
// normalizes to set_config($2, $1, $3) while an all-literal one-GUC decoy
// normalizes to set_config($1, $2, $3). Those are different trees, so only a
// decoy that also carries a bind parameter in the same position collides.
func TestAdditiveSameShapeDecoysLandInTheirObservationalClass(t *testing.T) {
	manifest := testManifest(t)

	// Two-GUC: an all-literal decoy is structurally identical to the safety
	// pin, because the safety pin has no bind parameter either.
	twoGUC, err := StrictASTDigest(`SELECT pg_catalog.set_config('a', 'b', true), pg_catalog.set_config('c', 'd', true)`)
	if err != nil {
		t.Fatal(err)
	}
	if class := manifest.Classify(twoGUC, true); class != V3SafetySessionPin {
		t.Fatalf("an arbitrary two-GUC set_config classified as %s; it is structurally the safety pin, "+
			"and is caught by over-count rather than by classification", class)
	}

	// One-GUC with a bind parameter in the timeout pin's position: collides.
	boundOneGUC, err := StrictASTDigest(`SELECT pg_catalog.set_config('work_mem', $1, true)`)
	if err != nil {
		t.Fatal(err)
	}
	if class := manifest.Classify(boundOneGUC, true); class != V3StatementTimeoutPin {
		t.Fatalf("a one-GUC set_config with a bind parameter classified as %s; "+
			"it is structurally the timeout pin", class)
	}

	// One-GUC with no bind parameter: the parameter numbering differs, so this
	// one IS distinguishable and is rejected outright rather than by count.
	literalOneGUC, err := StrictASTDigest(`SELECT pg_catalog.set_config('work_mem', '64MB', true)`)
	if err != nil {
		t.Fatal(err)
	}
	if class := manifest.Classify(literalOneGUC, true); class != V3Unexpected {
		t.Fatalf("an all-literal one-GUC set_config classified as %s; the timeout pin carries a bind "+
			"parameter, so the two normalize to different parameter numbering and must not collide", class)
	}
}

// Case 2b: a constant-only replacement is observationally identical. The
// manifest cannot reject it, and must not pretend to -- but its source digest
// changes, which is the layer that does reject it.
func TestConstantOnlyReplacementIsObservationallyIdenticalButChangesTheSourceDigest(t *testing.T) {
	mutated := strings.Replace(dataconnector.SafetySessionPinSQL, "'pg_catalog'", "'public'", 1)
	if mutated == dataconnector.SafetySessionPinSQL {
		t.Fatal("the mutation did not change the statement")
	}
	original, err := StrictASTDigest(dataconnector.SafetySessionPinSQL)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := StrictASTDigest(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if original != replaced {
		t.Fatal("the strict AST digest distinguished a constant-only replacement; " +
			"pg_stat_statements erases those constants, so this test no longer models reality")
	}
	// The layer that does catch it.
	if sourceDigest(mutated) == sourceDigest(dataconnector.SafetySessionPinSQL) {
		t.Fatal("a constant-only replacement did not change the Connector source digest; " +
			"nothing would reject it")
	}
}

func TestClassifierManifestRejectsInvalidShapes(t *testing.T) {
	for name, mutate := range map[string]func(*ClassifierManifest){
		"version cleared": func(m *ClassifierManifest) { m.Version = "" },
		"no entries":      func(m *ClassifierManifest) { m.Entries = nil },
		"an unknown class": func(m *ClassifierManifest) {
			m.Entries[0].Class = "not_a_class"
		},
		"the unexpected class defined": func(m *ClassifierManifest) {
			m.Entries[0].Class = V3Unexpected
		},
		"one key mapped to two classes": func(m *ClassifierManifest) {
			m.Entries[1].StrictASTSHA256 = m.Entries[0].StrictASTSHA256
			m.Entries[1].RequiredTopLevel = m.Entries[0].RequiredTopLevel
		},
		"a missing required class": func(m *ClassifierManifest) {
			trimmed := make([]ClassifierEntry, 0, len(m.Entries))
			for _, entry := range m.Entries {
				if entry.Class != V3RepresentationPin {
					trimmed = append(trimmed, entry)
				}
			}
			m.Entries = trimmed
		},
		"entries out of canonical order": func(m *ClassifierManifest) {
			m.Entries[0], m.Entries[len(m.Entries)-1] = m.Entries[len(m.Entries)-1], m.Entries[0]
		},
		"a Connector constant with no source digest": func(m *ClassifierManifest) {
			for index := range m.Entries {
				if m.Entries[index].SourceKind == SourceConnectorConstant {
					m.Entries[index].SourceSHA256 = ""
					return
				}
			}
		},
		"a runtime template bound to no PostgreSQL version": func(m *ClassifierManifest) {
			for index := range m.Entries {
				if m.Entries[index].SourceKind == SourceRuntimeTemplate {
					m.Entries[index].PostgreSQLVersionNum = 0
					return
				}
			}
		},
		"a target naming no frozen contract": func(m *ClassifierManifest) {
			for index := range m.Entries {
				if m.Entries[index].SourceKind == SourceQueryContract {
					m.Entries[index].ContractIdentity = ""
					return
				}
			}
		},
		"an unknown source kind": func(m *ClassifierManifest) {
			m.Entries[0].SourceKind = "invented"
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := testManifest(t)
			mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("an invalid classifier manifest was accepted")
			}
			if _, err := manifest.SHA256(); err == nil {
				t.Fatal("an invalid classifier manifest produced a digest")
			}
		})
	}
}

// Targets are per operation. A different operation's rendered target must not
// classify, or a missing legitimate target could be replaced by another
// workload's allowed one with no class count changing.
func TestOperationSpecificTargetsDoNotAcceptAnotherWorkloadsQuery(t *testing.T) {
	manifest := testManifest(t)
	otherWorkload, err := StrictASTDigest(
		`SELECT receipt_no, department FROM reporting.expense_detail WHERE department = ANY ($1)`)
	if err != nil {
		t.Fatal(err)
	}
	if class := manifest.Classify(otherWorkload, true); class != V3Unexpected {
		t.Fatalf("another workload's target classified as %s in this operation's manifest", class)
	}
}

func TestBuildClassifierManifestRejectsNonTargetExtras(t *testing.T) {
	control := ClassifierEntry{
		Class: V3SafetySessionPin, StrictASTSHA256: testSchemaDigest,
		RequiredTopLevel: true, SourceKind: SourceQueryContract, ContractIdentity: "x",
	}
	if _, err := BuildClassifierManifest(qualifiedFootprint(t), []ClassifierEntry{control}); err == nil {
		t.Fatal("a non-target entry was accepted as an operation target")
	}
	unpinned := ClassifierEntry{
		Class: V3TargetedVisible, StrictASTSHA256: testSchemaDigest,
		RequiredTopLevel: true, SourceKind: SourceConnectorConstant, SourceSHA256: testSchemaDigest,
	}
	if _, err := BuildClassifierManifest(qualifiedFootprint(t), []ClassifierEntry{unpinned}); err == nil {
		t.Fatal("a target not bound to a frozen query contract was accepted")
	}
}
