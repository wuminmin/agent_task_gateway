package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// These assertions make the test package fail to compile if the public ends of
// the prepared-member enum stop describing the 31-member closed interval that
// the rejection wire is required to cover. PreparedOperationMembers is checked
// against the exact code set at runtime below.
const expectedPreparedOperationMemberCount = 31

var _ [int(preparedbinding.PreparedMemberHasCompanion) - 1]struct{}
var _ [1 - int(preparedbinding.PreparedMemberHasCompanion)]struct{}
var _ [int(preparedbinding.PreparedMemberCompanionTarget) - expectedPreparedOperationMemberCount]struct{}
var _ [expectedPreparedOperationMemberCount - int(preparedbinding.PreparedMemberCompanionTarget)]struct{}

func TestTaskGateRejectionV1ClosedEnumTables(t *testing.T) {
	tables := []struct {
		name  string
		names []string
		count int
		parse func(string) (int, error)
	}{
		{"phase", rejectionPhaseNames[:], int(rejectionPhaseCount), func(value string) (int, error) {
			parsed, err := parseRejectionPhase(value)
			return int(parsed), err
		}},
		{"gate_code", rejectionGateNames[:], int(rejectionGateCount), func(value string) (int, error) {
			parsed, err := parseRejectionGate(value)
			return int(parsed), err
		}},
		{"failure_kind", rejectionFailureKindNames[:], int(rejectionFailureKindCount), func(value string) (int, error) {
			parsed, err := parseRejectionFailureKind(value)
			return int(parsed), err
		}},
		{"source", rejectionSourceNames[:], int(rejectionSourceCount), func(value string) (int, error) {
			parsed, err := parseRejectionSource(value)
			return int(parsed), err
		}},
		{"path_kind", rejectionPathNames[:], int(rejectionPathCount), func(value string) (int, error) {
			parsed, err := parseRejectionPathKind(value)
			return int(parsed), err
		}},
		{"target_role", rejectionTargetRoleNames[:], int(rejectionTargetRoleCount), func(value string) (int, error) {
			parsed, err := parseRejectionTargetRole(value)
			return int(parsed), err
		}},
		{"statement_class", rejectionStatementClassNames[:], int(rejectionStatementClassCount), func(value string) (int, error) {
			parsed, err := parseRejectionStatementClass(value)
			return int(parsed), err
		}},
		{"difference_kind", rejectionDifferenceKindNames[:], int(rejectionDifferenceKindCount), func(value string) (int, error) {
			return parseEnum(value, "difference kind", rejectionDifferenceKindNames[:])
		}},
		{"difference_field", rejectionDifferenceFieldNames[:], int(rejectionDifferenceFieldCount), func(value string) (int, error) {
			parsed, err := parseRejectionDifferenceField(value)
			return int(parsed), err
		}},
	}

	closedCode := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			if len(table.names) != table.count {
				t.Fatalf("wire-name cardinality %d differs from enum cardinality %d", len(table.names), table.count)
			}
			if table.names[0] != "" {
				t.Fatalf("invalid zero enum has wire spelling %q", table.names[0])
			}
			seen := make(map[string]bool, len(table.names)-1)
			for value, name := range table.names[1:] {
				value++
				if !closedCode.MatchString(name) {
					t.Errorf("enum %d has non-closed wire spelling %q", value, name)
				}
				if seen[name] {
					t.Errorf("wire spelling %q is duplicated", name)
				}
				seen[name] = true
				parsed, err := table.parse(name)
				if err != nil || parsed != value {
					t.Errorf("parse %q = %d, %v; want %d", name, parsed, err, value)
				}
			}
			for _, unknown := range []string{"", "unknown", "UPPERCASE", "future-enum"} {
				if parsed, err := table.parse(unknown); err == nil || parsed != 0 {
					t.Errorf("unknown spelling %q parsed as %d without error", unknown, parsed)
				}
			}
		})
	}

	for gate := rejectionGate(1); gate < rejectionGateCount; gate++ {
		if phase := phaseForRejectionGate(gate); phase <= rejectionPhaseInvalid || phase >= rejectionPhaseCount {
			t.Errorf("closed gate %q has no closed owning phase", enumName(rejectionGateNames[:], int(gate)))
		}
	}

	wantPrepared := expectedPreparedMemberCodes()
	gotPrepared := make([]string, 0, len(preparedbinding.PreparedOperationMembers()))
	seenPrepared := make(map[string]bool, len(wantPrepared))
	for _, member := range preparedbinding.PreparedOperationMembers() {
		code := member.Code()
		if code == "" || !closedCode.MatchString(code) || seenPrepared[code] {
			t.Errorf("prepared member %d has invalid or duplicate code %q", member, code)
		}
		seenPrepared[code] = true
		parsed, err := parsePreparedOperationMember(code)
		if err != nil || parsed != member {
			t.Errorf("parse prepared member %q = %d, %v; want %d", code, parsed, err, member)
		}
		gotPrepared = append(gotPrepared, code)
	}
	sort.Strings(gotPrepared)
	if !reflect.DeepEqual(gotPrepared, wantPrepared) {
		t.Fatalf("prepared-member enum changed\n got: %v\nwant: %v", gotPrepared, wantPrepared)
	}
	if preparedbinding.PreparedOperationMember(0).Code() != "" ||
		preparedbinding.PreparedOperationMember(255).Code() != "" {
		t.Fatal("an out-of-range prepared member has a wire spelling")
	}
	for _, class := range GatewayStatementClassesV3() {
		mapped, ok := rejectionStatementClassForGateway(class)
		if !ok || enumName(rejectionStatementClassNames[:], int(mapped)) != string(class) {
			t.Errorf("gateway statement class %q has no exact closed rejection mapping", class)
		}
	}
	if _, ok := rejectionStatementClassForGateway(GatewayStatementClassV3("future_class")); ok {
		t.Fatal("an unknown gateway statement class entered the rejection enum")
	}
}

func TestTaskGateRejectionV1StorageIsOpaqueAndCredentialFree(t *testing.T) {
	for _, structure := range []reflect.Type{
		reflect.TypeOf(TaskGateRejectionV1{}),
		reflect.TypeOf(rejectionDifferenceV1{}),
		reflect.TypeOf(rejectionSHA256{}),
	} {
		assertOpaqueRejectionStorageType(t, structure, make(map[reflect.Type]bool))
	}
}

func TestTaskGateRejectionV1EveryClosedGateHasAProductionClassification(t *testing.T) {
	directory := filepath.Join(repositoryRoot(t), "evaluation", "internal", "experiment")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	used := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "taskgate_rejection_v1.go" {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok && strings.HasPrefix(identifier.Name, "rejectionGate") {
				used[identifier.Name] = true
			}
			return true
		})
	}
	for gate := rejectionGate(1); gate < rejectionGateCount; gate++ {
		name := rejectionGateIdentifier(gate)
		if name == "" {
			t.Fatalf("test has no identifier mapping for closed gate %q", enumName(rejectionGateNames[:], int(gate)))
		}
		if !used[name] {
			t.Errorf("closed gate %s has no production classification point", name)
		}
	}
}

func rejectionGateIdentifier(gate rejectionGate) string {
	code := enumName(rejectionGateNames[:], int(gate))
	if code == "" {
		return ""
	}
	parts := strings.Split(code, "_")
	var name strings.Builder
	name.WriteString("rejectionGate")
	for _, part := range parts {
		if part == "postgresql" {
			name.WriteString("PostgreSQL")
			continue
		}
		name.WriteString(strings.ToUpper(part[:1]))
		name.WriteString(part[1:])
	}
	return name.String()
}

func TestTaskGateRejectionV1ProductionClassificationsRemainReachable(t *testing.T) {
	_, materialErr := reproduceForPath(queryreceipt.QueryReceiptV1{}, PathPairedNovel, TrustedInputsV3{})
	assertTaskGateRejectionGate(t, materialErr, rejectionGateMaterialPresence)

	limitErr := requireDerivedLimitsSignedV3(physicalquery.Limits{VisibleRowLimit: 1},
		querybinding.QueryExecutionBindingV2{})
	assertTaskGateRejectionGate(t, limitErr, rejectionGateDerivedLimits)

	carriedErr := requireCarriedMatchesSigned(rejectionTargetRoleVisible,
		querybinding.TargetRecordV1{ExactSQLSHA256: strings.Repeat("a", 64)},
		physicalquery.StatementIdentity{ExactSHA256: strings.Repeat("b", 64)}, "")
	assertTaskGateRejectionGate(t, carriedErr, rejectionGateCarriedTargets)

	plan := testPlan(t, PathPairedNovel)
	unexpectedErr := (ObservedDelta{Unexpected: []ObserverStructuralRow{{
		StrictASTSHA256: strings.Repeat("c", 64), TopLevel: true, Calls: 1,
	}}}).Accept(plan)
	assertTaskGateRejectionGate(t, unexpectedErr, rejectionGateUnexpectedStructuralStatements)

	perClass := plan.Expected()
	internalErr := (ObservedDelta{PerClass: perClass, Total: plan.ExpectedTotal()}).Accept(plan)
	assertTaskGateRejectionGate(t, internalErr, rejectionGateInternalStatementMultiset)

	acceptedInternal := append([]InternalExpectation(nil), plan.InternalExpectation...)
	totalErr := (ObservedDelta{
		PerClass: perClass, Internal: acceptedInternal, Total: plan.ExpectedTotal() + 1,
	}).Accept(plan)
	assertTaskGateRejectionGate(t, totalErr, rejectionGateObserverTotal)

	window, classifier, _ := pairedNovelWindow(t)
	window.Before.ClassifierManifestSHA256 = strings.Repeat("d", 64)
	window.After.ClassifierManifestSHA256 = window.Before.ClassifierManifestSHA256
	_, commitmentErr := window.Delta(classifier)
	assertTaskGateRejectionGate(t, commitmentErr, rejectionGateClassifierCommitment)
}

func TestTaskGateRejectionV1IdempotentReplayRetainsTruthfulInnerClassification(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*gateCase)
		wantFailure    rejectionFailureKind
		wantExpected   rejectionSource
		wantActual     rejectionSource
		wantDifference []rejectionDifferenceField
	}{
		{
			name: "invalid control-store evidence",
			mutate: func(broken *gateCase) {
				broken.trusted.Replay.TaskID = ""
			},
			wantFailure:  rejectionFailureInvalidValue,
			wantExpected: rejectionSourceControlStore,
			wantActual:   rejectionSourceControlStore,
		},
		{
			name: "settlement contradicts control-store evidence",
			mutate: func(broken *gateCase) {
				broken.trusted.SettlementWroteExecutionBindingRow = true
			},
			wantFailure:    rejectionFailureMismatch,
			wantExpected:   rejectionSourceControlStore,
			wantActual:     rejectionSourceControlStore,
			wantDifference: []rejectionDifferenceField{rejectionDifferenceActualBool, rejectionDifferenceExpectedBool},
		},
		{
			name: "returned bytes differ from persisted bytes",
			mutate: func(broken *gateCase) {
				broken.trusted.Replay.ReturnedReceiptJSON = append(
					append([]byte(nil), broken.trusted.Replay.ReturnedReceiptJSON...), ' ')
			},
			wantFailure:  rejectionFailureMismatch,
			wantExpected: rejectionSourceControlStore,
			wantActual:   rejectionSourceCarriedEvidence,
		},
		{
			name: "persisted digest contradicts persisted bytes",
			mutate: func(broken *gateCase) {
				broken.trusted.Replay.PersistedReceiptSHA256 = strings.Repeat("a", 64)
			},
			wantFailure:    rejectionFailureInvalidValue,
			wantExpected:   rejectionSourceFinalizerDerivation,
			wantActual:     rejectionSourceControlStore,
			wantDifference: []rejectionDifferenceField{rejectionDifferenceActualSHA256, rejectionDifferenceExpectedSHA256},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broken := idempotentReplayCase(t)
			test.mutate(&broken)
			_, err := broken.finalize()
			if err == nil {
				t.Fatal("broken idempotent replay was accepted")
			}
			rejection, ok := TaskGateRejectionFromError(err)
			if !ok {
				t.Fatalf("idempotent replay rejection was not typed: %v", err)
			}
			if rejection.gate != rejectionGateReplayEvidence || rejection.failure != test.wantFailure ||
				rejection.expectedSource != test.wantExpected || rejection.actualSource != test.wantActual {
				t.Fatalf("wrong idempotent replay classification: gate=%q failure=%q expected=%q actual=%q",
					enumName(rejectionGateNames[:], int(rejection.gate)),
					enumName(rejectionFailureKindNames[:], int(rejection.failure)),
					enumName(rejectionSourceNames[:], int(rejection.expectedSource)),
					enumName(rejectionSourceNames[:], int(rejection.actualSource)))
			}
			if err := rejection.Validate(); err != nil {
				t.Fatalf("idempotent replay classification does not validate: %v", err)
			}
			gotFields := make([]rejectionDifferenceField, 0, len(rejection.differences))
			for _, difference := range rejection.differences {
				gotFields = append(gotFields, difference.field)
			}
			if len(gotFields) != len(test.wantDifference) {
				t.Fatalf("wrong idempotent replay differences: got %v want %v", gotFields, test.wantDifference)
			}
			for index := range gotFields {
				if gotFields[index] != test.wantDifference[index] {
					t.Fatalf("wrong idempotent replay differences: got %v want %v", gotFields, test.wantDifference)
				}
			}
			if len(gotFields) == 2 && gotFields[0] == rejectionDifferenceActualBool {
				if !rejection.differences[0].boolValue || rejection.differences[1].boolValue {
					t.Fatalf("settlement contradiction lost expected=false/actual=true: %+v", rejection.differences)
				}
			}
			if len(gotFields) == 2 && gotFields[0] == rejectionDifferenceActualSHA256 {
				expected := fmt.Sprintf("%x", sha256.Sum256(broken.trusted.Replay.PersistedReceiptJSON))
				if rejection.differences[0].sha256Value.String() != broken.trusted.Replay.PersistedReceiptSHA256 ||
					rejection.differences[1].sha256Value.String() != expected {
					t.Fatalf("persisted-digest contradiction lost its typed digest pair: %+v", rejection.differences)
				}
			}
		})
	}
}

func assertTaskGateRejectionGate(t *testing.T, err error, want rejectionGate) {
	t.Helper()
	rejection, ok := TaskGateRejectionFromError(err)
	if !ok {
		t.Fatalf("error %v has no typed TaskGate rejection", err)
	}
	if rejection.gate != want {
		t.Fatalf("rejection gate = %s, want %s", enumName(rejectionGateNames[:], int(rejection.gate)),
			enumName(rejectionGateNames[:], int(want)))
	}
}

func assertOpaqueRejectionStorageType(t *testing.T, structure reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[structure] {
		return
	}
	seen[structure] = true
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if structure.Implements(errorType) || (structure.Kind() != reflect.Pointer && reflect.PointerTo(structure).Implements(errorType)) {
		t.Errorf("durable rejection storage type %s implements error", structure)
	}
	switch structure.Kind() {
	case reflect.String, reflect.Map, reflect.Interface:
		t.Errorf("durable rejection storage reaches forbidden %s type %s", structure.Kind(), structure)
	case reflect.Pointer:
		t.Errorf("durable rejection storage reaches pointer type %s", structure)
	case reflect.Slice, reflect.Array:
		assertOpaqueRejectionStorageType(t, structure.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < structure.NumField(); index++ {
			field := structure.Field(index)
			if field.PkgPath == "" {
				t.Errorf("durable rejection storage %s exposes field %s", structure, field.Name)
			}
			assertOpaqueRejectionStorageType(t, field.Type, seen)
		}
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// Closed enum, boolean, count, and digest byte storage.
	default:
		t.Errorf("durable rejection storage reaches unapproved kind %s through %s", structure.Kind(), structure)
	}
}

func TestTaskGateRejectionV1PreparedMismatchNamesAllMembers(t *testing.T) {
	left := fullPreparedBindingForRejectionTest(t, "left", true, true, true, 4, 2, 3, 4096)
	right := fullPreparedBindingForRejectionTest(t, "right", false, false, false, 5, 3, 4, 4097)

	mismatchErr := left.RequireSame(right)
	var mismatch *preparedbinding.PreparedOperationMismatchError
	if !errors.As(mismatchErr, &mismatch) {
		t.Fatalf("RequireSame returned %T, %v; want PreparedOperationMismatchError", mismatchErr, mismatchErr)
	}
	mismatchCodes := preparedMemberCodes(mismatch.Members())
	want := expectedPreparedMemberCodes()
	if !reflect.DeepEqual(mismatchCodes, want) {
		t.Fatalf("whole-binding mismatch omitted or renamed members\n got: %v\nwant: %v", mismatchCodes, want)
	}

	rejection, ok := rejectionFromPreparedMismatch(fmt.Errorf("reproduction refused: %w", mismatchErr))
	if !ok {
		t.Fatal("typed prepared mismatch did not produce a rejection")
	}
	encoded := mustMarshalRejection(t, rejection)
	var wire taskGateRejectionWireV1
	if err := StrictJSON(encoded, &wire); err != nil {
		t.Fatalf("decode rejection wire: %v", err)
	}
	if wire.GateCode != "prepared_binding_members" || wire.FailureKind != "mismatch" {
		t.Fatalf("prepared mismatch emitted gate=%q failure=%q", wire.GateCode, wire.FailureKind)
	}
	var evidenceCodes []string
	for _, difference := range wire.Differences {
		if difference.Field != "prepared_member" || difference.Enum == nil {
			t.Fatalf("prepared mismatch emitted non-member difference: %+v", difference)
		}
		evidenceCodes = append(evidenceCodes, *difference.Enum)
	}
	if !reflect.DeepEqual(evidenceCodes, want) {
		t.Fatalf("rejection did not retain the complete member list\n got: %v\nwant: %v", evidenceCodes, want)
	}
}

func TestTaskGateRejectionV1CandidateZeroMatchShape(t *testing.T) {
	rejection := candidateZeroMatchRejectionForTest(t, 4)
	encoded := mustMarshalRejection(t, rejection)
	var wire taskGateRejectionWireV1
	if err := StrictJSON(encoded, &wire); err != nil {
		t.Fatalf("decode candidate rejection: %v", err)
	}
	if wire.Version != TaskGateRejectionV1Version ||
		wire.Phase != "contract_identification" ||
		wire.GateCode != "candidate_match_count" ||
		wire.FailureKind != "mismatch" ||
		wire.ExpectedSource != "frozen_contract" ||
		wire.ActualSource != "gateway_receipt" {
		t.Fatalf("candidate zero-match identity is not closed: %+v", wire)
	}
	want := map[string]int64{
		"candidate_count":         4,
		"matched_candidate_count": 0,
		"refused_candidate_count": 4,
	}
	got := make(map[string]int64, len(wire.Differences))
	for _, difference := range wire.Differences {
		if difference.Count == nil || difference.Bool != nil || difference.LowercaseSHA256 != nil || difference.Enum != nil {
			t.Fatalf("candidate zero-match difference is not a count: %+v", difference)
		}
		got[difference.Field] = *difference.Count
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate zero-match counts = %v; want %v", got, want)
	}
}

func TestTaskGateRejectionV1UnexpectedRetainsFullStructuralKeysAndCounts(t *testing.T) {
	rows := []ObserverStructuralRow{
		{StrictASTSHA256: strings.Repeat("1", 64), TopLevel: true, Calls: 2},
		{StrictASTSHA256: strings.Repeat("a", 64), TopLevel: false, Calls: 7},
		{StrictASTSHA256: strings.Repeat("deadbeef", 8), TopLevel: true, Calls: 11},
	}
	rejection := newTaskGateRejectionV1(rejectionGateUnexpectedStructuralStatements,
		rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
		rejectionUnexpectedDifferences(rows)...)
	rejection.statementClass = rejectionStatementUnexpected
	encoded := mustMarshalRejection(t, rejection)
	var wire taskGateRejectionWireV1
	if err := StrictJSON(encoded, &wire); err != nil {
		t.Fatalf("decode unexpected rejection: %v", err)
	}
	if wire.GateCode != "unexpected_structural_statements" || wire.Phase != "closed_world_accounting" {
		t.Fatalf("unexpected rows emitted gate=%q phase=%q", wire.GateCode, wire.Phase)
	}
	if wire.StatementClass == nil || *wire.StatementClass != "unexpected" {
		t.Fatalf("unexpected rows emitted statement_class=%v", wire.StatementClass)
	}

	type structuralKey struct {
		digest   string
		topLevel bool
		calls    int64
	}
	gotRows := make(map[int64]*structuralKey, len(rows))
	counts := make(map[string]int64)
	for _, difference := range wire.Differences {
		if difference.Ordinal == nil {
			if difference.Count != nil {
				counts[difference.Field] = *difference.Count
			}
			continue
		}
		row := gotRows[*difference.Ordinal]
		if row == nil {
			row = &structuralKey{}
			gotRows[*difference.Ordinal] = row
		}
		switch difference.Field {
		case "unexpected_statement_sha256":
			if difference.LowercaseSHA256 == nil {
				t.Fatalf("unexpected digest is not typed: %+v", difference)
			}
			row.digest = *difference.LowercaseSHA256
		case "unexpected_statement_toplevel":
			if difference.Bool == nil {
				t.Fatalf("unexpected toplevel is not typed: %+v", difference)
			}
			row.topLevel = *difference.Bool
		case "unexpected_statement_calls":
			if difference.Count == nil {
				t.Fatalf("unexpected calls is not typed: %+v", difference)
			}
			row.calls = *difference.Count
		default:
			t.Fatalf("unexpected row emitted field %q", difference.Field)
		}
	}
	if counts["expected_count"] != 0 || counts["actual_count"] != int64(len(rows)) {
		t.Fatalf("unexpected row totals = %v", counts)
	}
	for ordinal, want := range rows {
		got := gotRows[int64(ordinal)]
		if got == nil || got.digest != want.StrictASTSHA256 || got.topLevel != want.TopLevel || got.calls != want.Calls {
			t.Errorf("unexpected row %d = %+v; want digest=%s toplevel=%t calls=%d",
				ordinal, got, want.StrictASTSHA256, want.TopLevel, want.Calls)
		}
		if got != nil && len(got.digest) != 64 {
			t.Errorf("unexpected row %d retained truncated digest %q", ordinal, got.digest)
		}
	}
}

func TestTaskGateRejectionV1RequiredDifferenceShapesFailClosed(t *testing.T) {
	preparedMissing := newTaskGateRejectionV1(rejectionGatePreparedBindingMembers,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt)
	if err := preparedMissing.Validate(); err == nil {
		t.Fatal("prepared-binding rejection accepted an empty member list")
	}

	candidateMissing := newTaskGateRejectionV1(rejectionGateCandidateMatchCount,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
		rejectionCountDifference(rejectionDifferenceCandidateCount, 1),
		rejectionCountDifference(rejectionDifferenceMatchedCandidateCount, 0))
	if err := candidateMissing.Validate(); err == nil {
		t.Fatal("zero-match rejection accepted an incomplete candidate census")
	}
	candidateWrongRefused := newTaskGateRejectionV1(rejectionGateCandidateMatchCount,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
		rejectionCountDifference(rejectionDifferenceCandidateCount, 2),
		rejectionCountDifference(rejectionDifferenceRefusedCandidateCount, 1),
		rejectionCountDifference(rejectionDifferenceMatchedCandidateCount, 0))
	if err := candidateWrongRefused.Validate(); err == nil {
		t.Fatal("zero-match rejection accepted inconsistent candidate/refusal counts")
	}

	row := ObserverStructuralRow{StrictASTSHA256: strings.Repeat("a", 64), TopLevel: true, Calls: 1}
	complete := newTaskGateRejectionV1(rejectionGateUnexpectedStructuralStatements,
		rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
		rejectionUnexpectedDifferences([]ObserverStructuralRow{row})...)
	if err := complete.Validate(); err != nil {
		t.Fatalf("complete unexpected-structural rejection does not validate: %v", err)
	}
	for _, field := range []rejectionDifferenceField{
		rejectionDifferenceUnexpectedStatementSHA256,
		rejectionDifferenceUnexpectedStatementTopLevel,
		rejectionDifferenceUnexpectedStatementCalls,
	} {
		incomplete := complete
		incomplete.differences = nil
		for _, difference := range complete.differences {
			if difference.field != field {
				incomplete.differences = append(incomplete.differences, difference)
			}
		}
		if err := incomplete.Validate(); err == nil {
			t.Errorf("unexpected-structural rejection accepted a row without %s",
				enumName(rejectionDifferenceFieldNames[:], int(field)))
		}
	}

	duplicateLogical := newTaskGateRejectionV1(rejectionGateObserverTotal,
		rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
		rejectionCountDifference(rejectionDifferenceExpectedCount, 1),
		rejectionCountDifference(rejectionDifferenceExpectedCount, 2))
	if err := duplicateLogical.Validate(); err == nil {
		t.Fatal("rejection accepted two values for one logical difference field")
	}

	maxCalls := newTaskGateRejectionV1(rejectionGateUnexpectedStructuralStatements,
		rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
		rejectionUnexpectedDifferences([]ObserverStructuralRow{{
			StrictASTSHA256: strings.Repeat("b", 64), TopLevel: false, Calls: maxTaskGateRejectionCount,
		}})...)
	if err := maxCalls.Validate(); err != nil {
		t.Fatalf("taxonomy cannot retain a valid nonnegative int64 call count: %v", err)
	}
	encoded := mustMarshalRejection(t, maxCalls)
	if !bytes.Contains(encoded, []byte(`"count":9223372036854775807`)) {
		t.Fatalf("maximum int64 call count was not retained exactly: %s", encoded)
	}
}

func TestTaskGateRejectionV1RejectsOpenOrAmbiguousJSON(t *testing.T) {
	base := mustMarshalRejection(t, candidateZeroMatchRejectionForTest(t, 4))
	prepared := newTaskGateRejectionV1(rejectionGatePreparedBindingMembers,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
		rejectionPreparedMemberDifferences([]preparedbinding.PreparedOperationMember{
			preparedbinding.PreparedMemberVisibleFields,
		})...)
	preparedJSON := mustMarshalRejection(t, prepared)
	digestDifference, err := rejectionSHA256Difference(rejectionDifferenceActualSHA256, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("construct digest difference: %v", err)
	}
	digestJSON := mustMarshalRejection(t, newTaskGateRejectionV1(rejectionGateStatementIdentity,
		rejectionFailureMismatch, rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence,
		digestDifference))

	invalid := map[string][]byte{
		"unknown top field": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["raw_error"] = "must not survive"
		}),
		"unknown nested field": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["sqlstate"] = "XX000"
		}),
		"unknown version": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["version"] = "taskgate-rejection-v2"
		}),
		"unknown phase": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["phase"] = "future_phase"
		}),
		"unknown gate": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["gate_code"] = "future_gate"
		}),
		"unknown failure": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["failure_kind"] = "future_failure"
		}),
		"unknown expected source": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["expected_source"] = "future_source"
		}),
		"unknown actual source": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["actual_source"] = "future_source"
		}),
		"unknown path enum": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["path_kind"] = "future_path"
		}),
		"unknown target enum": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["target_role"] = "future_target"
		}),
		"unknown class enum": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["statement_class"] = "future_class"
		}),
		"empty optional enum": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["path_kind"] = ""
		}),
		"null optional enum": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["path_kind"] = nil
		}),
		"unknown difference field": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["field"] = "future_difference"
		}),
		"unknown prepared enum": mutateRejectionJSON(t, preparedJSON, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["enum"] = "future_member"
		}),
		"uppercase SHA-256": mutateRejectionJSON(t, digestJSON, func(top map[string]any) {
			difference := top["differences"].([]any)[0].(map[string]any)
			difference["lowercase_sha256"] = strings.ToUpper(difference["lowercase_sha256"].(string))
		}),
		"union with two values": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["bool"] = false
		}),
		"union with null extra value": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["bool"] = nil
		}),
		"null ordinal": mutateRejectionJSON(t, base, func(top map[string]any) {
			top["differences"].([]any)[0].(map[string]any)["ordinal"] = nil
		}),
		"union with no value": mutateRejectionJSON(t, base, func(top map[string]any) {
			delete(top["differences"].([]any)[0].(map[string]any), "count")
		}),
		"duplicate logical difference": mutateRejectionJSON(t, base, func(top map[string]any) {
			differences := top["differences"].([]any)
			top["differences"] = append(differences, differences[0])
		}),
		"trailing value": append(append([]byte(nil), base...), []byte(` {}`)...),
		"trailing token": append(append([]byte(nil), base...), []byte(` sentinel`)...),
		"duplicate top member": bytes.Replace(base,
			[]byte(`"version":"taskgate-rejection-v1"`),
			[]byte(`"version":"taskgate-rejection-v1","version":"taskgate-rejection-v1"`), 1),
		"duplicate nested member": bytes.Replace(base,
			[]byte(`"field":"candidate_count"`),
			[]byte(`"field":"candidate_count","field":"candidate_count"`), 1),
	}
	for name, encoded := range invalid {
		t.Run(name, func(t *testing.T) {
			var rejection TaskGateRejectionV1
			if err := StrictJSON(encoded, &rejection); err == nil {
				t.Fatalf("accepted open or ambiguous rejection JSON: %s", encoded)
			}
		})
	}
}

func TestTaskGateRejectionV1SerializationExcludesSentinelSecretsAndErrorChain(t *testing.T) {
	rejection := candidateZeroMatchRejectionForTest(t, 4)
	want := mustMarshalRejection(t, rejection)
	sentinels := []string{
		"TASKGATE_RJ_SENTINEL_SECRET_a9ee5212",
		"postgres://taxonomy_user:Taxonomy-Only-Pass_8d7f@db.internal:5432/evidence?sslmode=disable",
		"password=Taxonomy-Only-Pass_8d7f",
		"-----BEGIN PRIVATE KEY-----\nVEFTS0dBVEVfU0VOVElORUw=\n-----END PRIVATE KEY-----",
		"SELECT * FROM private_taxonomy_table WHERE token = 'Taxonomy-Only-Pass_8d7f'",
		"/srv/private/taskgate/Taxonomy-Only-Pass_8d7f.json",
		`{"credential":"Taxonomy-Only-Pass_8d7f"}`,
	}
	cause := errors.New(strings.Join(sentinels, " | "))
	typed := withTaskGateRejection(cause, rejection)
	wrapped := fmt.Errorf("outer operator context %s: %w", sentinels[0], typed)
	chain := errors.Join(errors.New(sentinels[1]), wrapped)
	extracted, ok := TaskGateRejectionFromError(chain)
	if !ok {
		t.Fatal("typed rejection was lost in an error chain")
	}
	got := mustMarshalRejection(t, *extracted)
	if !bytes.Equal(got, want) {
		t.Fatalf("operator error chain changed durable rejection\n got: %s\nwant: %s", got, want)
	}
	assertCredentialFreeRejectionJSON(t, got, sentinels...)
	assertClosedRejectionWireVocabulary(t, got)
	if _, ok := TaskGateRejectionFromError(cause); ok {
		t.Fatal("an untyped operator error was treated as a durable rejection")
	}
}

func FuzzTaskGateRejectionV1Serialization(f *testing.F) {
	valid := mustMarshalRejection(f, candidateZeroMatchRejectionForTest(f, 4))
	f.Add(valid)
	f.Add([]byte(`{"version":"taskgate-rejection-v1","phase":"runtime_identity","gate_code":"finalizer_internal","failure_kind":"invalid","expected_source":"finalizer_derivation","actual_source":"finalizer_derivation"}`))
	f.Add([]byte(`{"version":"taskgate-rejection-v1","version":"taskgate-rejection-v1","phase":"runtime_identity","gate_code":"finalizer_internal","failure_kind":"invalid","expected_source":"finalizer_derivation","actual_source":"finalizer_derivation"}`))
	f.Add([]byte(`{"version":"taskgate-rejection-v1","phase":"runtime_identity","gate_code":"finalizer_internal","failure_kind":"invalid","expected_source":"finalizer_derivation","actual_source":"finalizer_derivation","raw_error":"TASKGATE_RJ_SENTINEL_SECRET_a9ee5212"}`))

	f.Fuzz(func(t *testing.T, input []byte) {
		var rejection TaskGateRejectionV1
		if err := StrictJSON(input, &rejection); err != nil {
			return
		}
		if err := rejection.Validate(); err != nil {
			t.Fatalf("decoder accepted rejection that does not validate: %v", err)
		}
		first, err := json.Marshal(rejection)
		if err != nil {
			t.Fatalf("marshal decoded rejection: %v", err)
		}
		assertClosedRejectionWireVocabulary(t, first)
		assertCredentialFreeRejectionJSON(t, first, "TASKGATE_RJ_SENTINEL_SECRET_a9ee5212")

		var roundTrip TaskGateRejectionV1
		if err := StrictJSON(first, &roundTrip); err != nil {
			t.Fatalf("strict round trip failed: %v", err)
		}
		second, err := json.Marshal(roundTrip)
		if err != nil {
			t.Fatalf("marshal round trip: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("rejection serialization is not canonical\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

func candidateZeroMatchRejectionForTest(tb testing.TB, candidates int64) TaskGateRejectionV1 {
	tb.Helper()
	rejection := newTaskGateRejectionV1(rejectionGateCandidateMatchCount,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
		rejectionCountDifference(rejectionDifferenceCandidateCount, candidates),
		rejectionCountDifference(rejectionDifferenceRefusedCandidateCount, candidates),
		rejectionCountDifference(rejectionDifferenceMatchedCandidateCount, 0))
	if err := rejection.Validate(); err != nil {
		tb.Fatalf("candidate zero-match fixture does not validate: %v", err)
	}
	return rejection
}

func fullPreparedBindingForRejectionTest(t *testing.T, prefix string,
	hasCompanion, grouped, expanded bool, visible, fact, provenance int, estimated uint64,
) preparedbinding.PreparedOperationBindingV1 {
	t.Helper()
	digest := func(member string) string {
		sum := sha256.Sum256([]byte(prefix + ":" + member))
		return hex.EncodeToString(sum[:])
	}
	binding := preparedbinding.PreparedOperationBindingV1{
		HasCompanion: hasCompanion, Grouped: grouped, ExpandedEvidence: expanded,
		VisibleFieldCount: visible, FactFieldCount: fact, ProvenanceFieldCount: provenance,
		VisibleFieldsSHA256: digest("visible fields"), FactFieldsSHA256: digest("fact fields"),
		ProvenanceFieldsSHA256:  digest("provenance fields"),
		PreparationInputsSHA256: digest("preparation inputs"), GrantSHA256: digest("grant"),
		CatalogSHA256: digest("catalog"), SnapshotBindingSetSHA256: digest("snapshot binding set"),
		PlanSHA256: digest("plan"), CompilerIdentitySHA256: digest("compiler identity"),
		PolicyGrantSHA256: digest("policy grant"), NormalFormSHA256: digest("normal form"),
		OrdinalProgramSHA256: digest("ordinal program"), DictionarySetSHA256: digest("dictionary set"),
		SidecarGrantsSHA256: digest("sidecar grants"), SourcePublicationsSHA256: digest("source publications"),
		ViewBindingSHA256: digest("view binding"), ViewRegistryRevisionSHA256: digest("view registry revision"),
		ViewArtifactSHA256: digest("view artifact"), ViewCompositionSHA256: digest("view composition"),
		TerminalProductClosureSHA256: digest("terminal product closure"),
		GovernanceEnvelopeSHA256:     digest("governance envelope"),
		PredicateFootprintSHA256:     digest("predicate footprint"), EstimatedBaseFacts: estimated,
		VisibleTargetSHA256: digest("visible target"),
	}
	if hasCompanion {
		binding.CompanionTargetSHA256 = digest("companion target")
	}
	sealed, err := binding.Seal()
	if err != nil {
		t.Fatalf("seal prepared binding: %v", err)
	}
	if err := sealed.Validate(); err != nil {
		t.Fatalf("prepared binding fixture does not validate: %v", err)
	}
	return sealed
}

func expectedPreparedMemberCodes() []string {
	return []string{
		"catalog",
		"companion_target",
		"compiler_identity",
		"dictionary_set",
		"estimated_base_facts",
		"expanded_evidence",
		"fact_field_count",
		"fact_fields",
		"governance_envelope",
		"grant",
		"grouped",
		"has_companion",
		"normal_form",
		"ordinal_program",
		"plan",
		"policy_grant",
		"predicate_footprint",
		"preparation_inputs",
		"provenance_field_count",
		"provenance_fields",
		"sidecar_grants",
		"snapshot_binding_set",
		"source_publications",
		"terminal_product_closure",
		"view_artifact",
		"view_binding",
		"view_composition",
		"view_registry_revision",
		"visible_field_count",
		"visible_fields",
		"visible_target",
	}
}

func preparedMemberCodes(members []preparedbinding.PreparedOperationMember) []string {
	result := make([]string, 0, len(members))
	for _, member := range members {
		result = append(result, member.Code())
	}
	sort.Strings(result)
	return result
}

func mustMarshalRejection(tb testing.TB, rejection TaskGateRejectionV1) []byte {
	tb.Helper()
	encoded, err := json.Marshal(rejection)
	if err != nil {
		tb.Fatalf("marshal rejection: %v", err)
	}
	return encoded
}

func mutateRejectionJSON(t *testing.T, encoded []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatalf("decode mutation fixture: %v", err)
	}
	mutate(top)
	mutated, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("encode mutation fixture: %v", err)
	}
	return mutated
}

func assertClosedRejectionWireVocabulary(t *testing.T, encoded []byte) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := StrictJSON(encoded, &top); err != nil {
		t.Fatalf("strict-decode serialized rejection: %v", err)
	}
	allowedTop := map[string]bool{
		"version": true, "phase": true, "gate_code": true, "failure_kind": true,
		"expected_source": true, "actual_source": true, "path_kind": true,
		"target_role": true, "statement_class": true, "differences": true,
	}
	for key := range top {
		if !allowedTop[key] {
			t.Errorf("serialized rejection contains open top-level field %q", key)
		}
	}
	var wire taskGateRejectionWireV1
	if err := StrictJSON(encoded, &wire); err != nil {
		t.Fatalf("decode serialized rejection wire: %v", err)
	}
	if wire.Version != TaskGateRejectionV1Version {
		t.Errorf("serialized rejection version = %q", wire.Version)
	}
	for label, value := range map[string]string{
		"phase": wire.Phase, "gate": wire.GateCode, "failure": wire.FailureKind,
		"expected source": wire.ExpectedSource, "actual source": wire.ActualSource,
	} {
		if value == "" {
			t.Errorf("serialized rejection has empty %s", label)
		}
	}
	preparedCodes := make(map[string]bool)
	for _, code := range expectedPreparedMemberCodes() {
		preparedCodes[code] = true
	}
	for _, difference := range wire.Differences {
		values := 0
		if difference.Bool != nil {
			values++
		}
		if difference.Count != nil {
			values++
		}
		if difference.LowercaseSHA256 != nil {
			values++
			if _, err := parseRejectionSHA256(*difference.LowercaseSHA256); err != nil {
				t.Errorf("serialized rejection has untyped digest %q: %v", *difference.LowercaseSHA256, err)
			}
		}
		if difference.Enum != nil {
			values++
			if !preparedCodes[*difference.Enum] {
				t.Errorf("serialized rejection has open enum %q", *difference.Enum)
			}
		}
		if values != 1 {
			t.Errorf("serialized difference %q has %d typed values", difference.Field, values)
		}
		if _, err := parseRejectionDifferenceField(difference.Field); err != nil {
			t.Errorf("serialized rejection has open difference field %q", difference.Field)
		}
	}
}

func assertCredentialFreeRejectionJSON(t *testing.T, encoded []byte, sentinels ...string) {
	t.Helper()
	var document any
	if err := StrictJSON(encoded, &document); err != nil {
		t.Fatalf("decode serialized rejection for credential scan: %v", err)
	}
	var scalars []string
	collectStringScalars(document, &scalars)
	for _, sentinel := range sentinels {
		variants := []string{sentinel, base64.StdEncoding.EncodeToString([]byte(sentinel))}
		sum := sha256.Sum256([]byte(sentinel))
		variants = append(variants, hex.EncodeToString(sum[:]))
		escaped, err := json.Marshal(sentinel)
		if err != nil {
			t.Fatalf("marshal sentinel: %v", err)
		}
		variants = append(variants, strings.Trim(string(escaped), `"`))
		for _, variant := range variants {
			if variant != "" && bytes.Contains(encoded, []byte(variant)) {
				t.Errorf("serialized rejection contains sentinel-derived bytes %q", variant)
			}
		}
		for _, scalar := range scalars {
			if scalar == sentinel {
				t.Errorf("serialized rejection contains a JSON scalar equal to sentinel %q", sentinel)
			}
		}
	}
	secretAssignment := regexp.MustCompile(`(?i)(password|passwd|secret|token|private[_-]?key|access[_-]?key|dsn)\s*[:=]`)
	for _, scalar := range scalars {
		if strings.Contains(scalar, "-----BEGIN ") || secretAssignment.MatchString(scalar) {
			t.Errorf("serialized rejection contains credential-shaped scalar %q", scalar)
		}
		parsed, err := url.Parse(scalar)
		if err == nil && parsed.User != nil {
			t.Errorf("serialized rejection contains URL userinfo in scalar %q", scalar)
		}
	}
}

func collectStringScalars(value any, result *[]string) {
	switch typed := value.(type) {
	case string:
		*result = append(*result, typed)
	case []any:
		for _, child := range typed {
			collectStringScalars(child, result)
		}
	case map[string]any:
		for _, child := range typed {
			collectStringScalars(child, result)
		}
	}
}
