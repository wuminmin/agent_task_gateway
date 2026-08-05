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

// testManifest is the paired-novel manifest: the one path whose closed world
// contains every control class, so the classification tests below can exercise
// all of them from one document.
func testManifest(t *testing.T) ClassifierManifest {
	t.Helper()
	return compiledTestManifest(t, PathPairedNovel, testTargets(t)...)
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
		again := testManifest(t)
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
		"an unknown path kind": func(m *ClassifierManifest) { m.PathKind = "paired_novel_v2" },
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
	footprint := qualifiedFootprint(t)
	plan := testPlan(t, PathPairedNovel)
	control := ClassifierEntry{
		Class: V3SafetySessionPin, StrictASTSHA256: testSchemaDigest,
		RequiredTopLevel: true, SourceKind: SourceQueryContract, ContractIdentity: "x",
	}
	if _, err := BuildClassifierManifestV2(plan, &footprint, []ClassifierEntry{control}); err == nil {
		t.Fatal("a non-target entry was accepted as an operation target")
	}
	unpinned := ClassifierEntry{
		Class: V3TargetedVisible, StrictASTSHA256: testSchemaDigest,
		RequiredTopLevel: true, SourceKind: SourceConnectorConstant, SourceSHA256: testSchemaDigest,
	}
	if _, err := BuildClassifierManifestV2(plan, &footprint, []ClassifierEntry{unpinned}); err == nil {
		t.Fatal("a target not bound to a frozen query contract was accepted")
	}
}

// The v2 rule. Which classes a manifest declares is settled by the
// independently derived plan, in both directions: a class the plan expects must
// be declared exactly, and a class it does not expect must not be declared at
// all.
//
// The second half is what version 1 had backwards, and it is not surplus
// tidiness. An entry for a class the path cannot produce makes that statement
// CLASSIFIABLE, so a control statement appearing where none should would be
// counted as a known class rather than landing in the unexpected sink.
func TestManifestClassSetIsDerivedFromThePlan(t *testing.T) {
	plan := testPlan(t, PathPairedNovel)

	t.Run("a class the plan expects must be declared", func(t *testing.T) {
		manifest := testManifest(t)
		trimmed := make([]ClassifierEntry, 0, len(manifest.Entries))
		for _, entry := range manifest.Entries {
			if entry.Class != V3RepresentationPin {
				trimmed = append(trimmed, entry)
			}
		}
		manifest.Entries = trimmed
		if err := manifest.Validate(); err != nil {
			t.Fatalf("the trimmed manifest is still structurally valid: %v", err)
		}
		if err := manifest.requireClassSet(plan); err == nil {
			t.Fatal("a manifest missing a class the plan expects was accepted")
		}
	})

	t.Run("a class the plan does not expect must not be declared", func(t *testing.T) {
		// The representation pin is the paired path's alone: Connector.Query
		// issues none. A single-query manifest declaring one would make that
		// statement classifiable on a path that cannot produce it.
		single := testPlan(t, PathSingleQuery)
		manifest := compiledTestManifest(t, PathSingleQuery, pairedTargets(t)[:1]...)
		for _, entry := range manifest.Entries {
			if entry.Class == V3RepresentationPin {
				t.Fatal("a single-query manifest declared the representation pin")
			}
		}
		digest, err := StrictASTDigest(dataconnector.RepresentationPinSQL)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Entries = append(manifest.Entries, ClassifierEntry{
			Class: V3RepresentationPin, StrictASTSHA256: digest, RequiredTopLevel: true,
			SourceKind: SourceConnectorConstant, SourceSHA256: sourceDigest(dataconnector.RepresentationPinSQL),
		})
		sortManifestEntries(manifest.Entries)
		if err := manifest.Validate(); err != nil {
			t.Fatalf("the extended manifest is still structurally valid: %v", err)
		}
		if err := manifest.requireClassSet(single); err == nil {
			t.Fatal("a manifest declaring a class the path cannot produce was accepted")
		}
		if _, err := CompileClassifierV2(testOperation(t, PathSingleQuery), single, manifest); err == nil {
			t.Fatal("a manifest declaring a class the path cannot produce compiled")
		}
	})

	t.Run("internal keys are compared key by key", func(t *testing.T) {
		manifest := testManifest(t)
		for index := range manifest.Entries {
			if manifest.Entries[index].Class == V3PostgreSQLInternalAttestation {
				manifest.Entries[index].StrictASTSHA256 = testInternalKeyB
			}
		}
		sortManifestEntries(manifest.Entries)
		if err := manifest.requireClassSet(plan); err == nil {
			t.Fatal("a substituted PostgreSQL-internal key was accepted; " +
				"the class count is identical and only a per-key comparison sees it")
		}
	})
}

// Each path's manifest declares exactly the structures the author's decision
// names for it, and nothing else.
func TestEachPathDeclaresItsOwnClosedWorld(t *testing.T) {
	for _, testCase := range []struct {
		kind    GatewayPathKind
		targets []ClassifierEntry
		want    []GatewayStatementClassV3
	}{
		{PathPairedNovel, pairedTargets(t), []GatewayStatementClassV3{
			V3TransactionBegin, V3TransactionCommit, V3SafetySessionPin, V3RepresentationPin,
			V3StatementTimeoutPin, V3DatasourceIdentity, V3ViewColumnAttestation,
			V3ViewDefinitionAttest, V3PostgreSQLInternalAttestation,
			V3TargetedVisible, V3TargetedCompanion,
		}},
		{PathSingleQuery, pairedTargets(t)[:1], []GatewayStatementClassV3{
			V3TransactionBegin, V3TransactionCommit, V3SafetySessionPin,
			V3StatementTimeoutPin, V3DatasourceIdentity, V3ViewColumnAttestation,
			V3ViewDefinitionAttest, V3PostgreSQLInternalAttestation, V3TargetedVisible,
		}},
		{PathSemanticReplay, nil, []GatewayStatementClassV3{
			V3DatasourceIdentity, V3ViewColumnAttestation, V3ViewDefinitionAttest,
			V3PostgreSQLInternalAttestation,
		}},
		{PathIdempotentReplay, nil, nil},
	} {
		t.Run(string(testCase.kind), func(t *testing.T) {
			manifest := compiledTestManifest(t, testCase.kind, testCase.targets...)
			declared := map[GatewayStatementClassV3]bool{}
			for _, entry := range manifest.Entries {
				declared[entry.Class] = true
			}
			for _, class := range testCase.want {
				if !declared[class] {
					t.Errorf("%s declares no %s", testCase.kind, class)
				}
				delete(declared, class)
			}
			for class := range declared {
				t.Errorf("%s declares %s, which its plan does not expect", testCase.kind, class)
			}
		})
	}
}

// The idempotent replay's manifest is a document, not a gap: it exists, it has a
// path kind, it digests, it compiles -- and its entry list is empty, so every
// Business statement in the window is unclassified.
func TestIdempotentReplayHasASignedZeroStatementManifest(t *testing.T) {
	manifest := compiledTestManifest(t, PathIdempotentReplay)
	if manifest.Version != ClassifierManifestVersion {
		t.Fatalf("the zero-statement manifest carries version %q", manifest.Version)
	}
	if manifest.PathKind != PathIdempotentReplay {
		t.Fatalf("the zero-statement manifest carries path_kind %q", manifest.PathKind)
	}
	if len(manifest.Entries) != 0 {
		t.Fatalf("the idempotent replay manifest declares %d entr(ies)", len(manifest.Entries))
	}
	digest, err := manifest.SHA256()
	if err != nil {
		t.Fatalf("the zero-statement manifest produced no digest: %v", err)
	}
	if len(digest) != 64 {
		t.Fatalf("the zero-statement manifest digest is not SHA-256: %q", digest)
	}
	classifier, err := compileTest(t, PathIdempotentReplay, manifest)
	if err != nil {
		t.Fatalf("the zero-statement manifest did not compile: %v", err)
	}
	// Every structure any other path may legitimately produce is unclassified
	// here, which is the whole content of the contract.
	for name, sql := range map[string]string{
		"BEGIN":                   runtimeBeginTemplate,
		"COMMIT":                  runtimeCommitTemplate,
		"the safety pin":          dataconnector.SafetySessionPinSQL,
		"the datasource identity": dataconnector.DatasourceIdentitySQL,
		"a view attestation":      dataconnector.ViewColumnAttestationSQL,
	} {
		key, err := StrictASTDigest(sql)
		if err != nil {
			t.Fatal(err)
		}
		if class := classifier.Classify(key, true); class != V3Unexpected {
			t.Errorf("%s classified as %s in an idempotent replay's closed world", name, class)
		}
	}
	if class := classifier.Classify(testInternalKeyA, false); class != V3Unexpected {
		t.Errorf("a qualified PostgreSQL-internal key classified as %s in an idempotent replay", class)
	}
}

// A manifest with no entries is a contract only where the plan expects nothing.
// Compiled against any path that reaches Business PostgreSQL it must fail.
func TestAnEmptyManifestCompilesOnlyForANonExecutingPath(t *testing.T) {
	empty := compiledTestManifest(t, PathIdempotentReplay)
	for _, kind := range []GatewayPathKind{PathPairedNovel, PathSingleQuery, PathSemanticReplay} {
		t.Run(string(kind), func(t *testing.T) {
			if _, err := compileTest(t, kind, empty); err == nil {
				t.Fatalf("an entry-less manifest compiled for %s", kind)
			}
			// Nor by relabelling it: the class set still has to come from the
			// plan, and every one of these paths expects statements.
			relabelled := empty
			relabelled.PathKind = kind
			if err := relabelled.Validate(); err == nil {
				t.Fatalf("an entry-less manifest relabelled %s was structurally accepted", kind)
			}
			if _, err := compileTest(t, kind, relabelled); err == nil {
				t.Fatalf("an entry-less manifest relabelled %s compiled", kind)
			}
		})
	}
}

// An idempotent replay must not be given attestation material through any door:
// not a footprint, not an internal key, not a target.
func TestIdempotentReplayRejectsAttestationAndTargetMaterial(t *testing.T) {
	plan := testPlan(t, PathIdempotentReplay)
	footprint := qualifiedFootprint(t)

	if _, err := BuildClassifierManifestV2(plan, &footprint, nil); err == nil {
		t.Fatal("an idempotent replay manifest was built with a qualified footprint")
	}
	if _, err := BuildClassifierManifestV2(plan, nil, pairedTargets(t)); err == nil {
		t.Fatal("an idempotent replay manifest was built with target entries")
	}

	// And a hand-assembled one carrying an internal key must not compile.
	digest, err := footprint.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	smuggled := ClassifierManifest{
		Version: ClassifierManifestVersion, PathKind: PathIdempotentReplay,
		Entries: []ClassifierEntry{{
			Class: V3PostgreSQLInternalAttestation, StrictASTSHA256: testInternalKeyA,
			RequiredTopLevel: false, SourceKind: SourceQualifiedFootprint, FootprintSHA256: digest,
		}},
	}
	if err := smuggled.requireClassSet(plan); err == nil {
		t.Fatal("an idempotent replay manifest carrying an internal key satisfied the plan")
	}
	if _, err := compileTest(t, PathIdempotentReplay, smuggled); err == nil {
		t.Fatal("an idempotent replay manifest carrying an internal key compiled")
	}
}

// A v1 manifest is historical development evidence. It must be rejected by name
// rather than silently reinterpreted under the v2 class rules, which mean
// something else.
func TestManifestV1IsRejectedByName(t *testing.T) {
	manifest := testManifest(t)
	manifest.Version = classifierManifestVersionV1
	err := manifest.Validate()
	if err == nil {
		t.Fatal("a v1 classifier manifest was accepted for v1.5 acceptance")
	}
	if !strings.Contains(err.Error(), classifierManifestVersionV1) {
		t.Fatalf("the rejection %q does not name the superseded schema", err)
	}
}

func sortManifestEntries(entries []ClassifierEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && manifestLess(entries[j], entries[j-1]); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
