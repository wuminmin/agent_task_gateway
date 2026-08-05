package physicalquery

import (
	"strings"
	"testing"
)

func completePrepared() PreparedOperationV1 {
	return PreparedOperationV1{
		Version:       PreparedOperationV1Version,
		VisibleSQL:    `SELECT "row_id" FROM "reporting"."final_v5_result_heavy" LIMIT 100`,
		CompanionSQL:  `SELECT "ordinal" FROM "taskgate_ordinal"."final_v5_result_heavy_v1" LIMIT 101`,
		HasCompanion:  true,
		VisibleFields: []string{"row_id"}, FactFields: []string{"row_id"},
		ProvenanceFields: []string{"ordinal"},
		Grouped:          false, ExpandedEvidence: true,
		PlanDigest: strings.Repeat("1", 64), CatalogDigest: strings.Repeat("2", 64),
		CompilerIdentity:              "queryplan-v7",
		OrdinalProgramDigest:          strings.Repeat("3", 64),
		DictionarySetDigest:           strings.Repeat("4", 64),
		SidecarGrantsDigest:           strings.Repeat("5", 64),
		PreparedOperationSHA256:       strings.Repeat("6", 64),
		PreparedVisibleTargetSHA256:   strings.Repeat("7", 64),
		PreparedCompanionTargetSHA256: strings.Repeat("8", 64),
	}
}

func TestACompletePreparationValidates(t *testing.T) {
	if err := completePrepared().Validate(); err != nil {
		t.Fatalf("a complete preparation was rejected: %v", err)
	}
}

// The companion is presence-coupled in both directions. A half-present companion
// is what would let "this path executes no companion" and "this path executes a
// companion nobody recorded" look the same.
func TestCompanionPresenceIsCoupledInBothDirections(t *testing.T) {
	for name, breakIt := range map[string]func(*PreparedOperationV1){
		"companion SQL without the flag": func(p *PreparedOperationV1) { p.HasCompanion = false },
		"the flag without companion SQL": func(p *PreparedOperationV1) { p.CompanionSQL = "" },
		"the flag without a binding": func(p *PreparedOperationV1) {
			p.PreparedCompanionTargetSHA256 = ""
		},
		"a binding without the flag": func(p *PreparedOperationV1) {
			p.HasCompanion, p.CompanionSQL = false, ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			prepared := completePrepared()
			breakIt(&prepared)
			if err := prepared.Validate(); err == nil {
				t.Fatal("a half-present companion was accepted")
			}
		})
	}
}

// Expanded evidence is a property of an operation that HAS a companion; without
// one there is no evidence row budget to expand.
func TestExpandedEvidenceRequiresACompanion(t *testing.T) {
	prepared := completePrepared()
	prepared.HasCompanion, prepared.CompanionSQL = false, ""
	prepared.PreparedCompanionTargetSHA256 = ""
	if err := prepared.Validate(); err == nil {
		t.Fatal("expanded evidence was accepted on an operation with no companion")
	}
}

// The whole point of the durable half is that it carries identities, never
// statements. A member added on the wrong side of the json:"-" line must be
// caught here rather than in whatever evidence file it eventually reaches.
func TestAPreparedOperationNeverSerializesSQL(t *testing.T) {
	leaks, err := completePrepared().CarriesSQL()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if leaks {
		t.Fatal("a prepared operation serialized statement text")
	}
}

func TestTwoIdenticalPreparationsAgree(t *testing.T) {
	if err := completePrepared().RequireSame(completePrepared()); err != nil {
		t.Fatalf("two identical preparations disagreed: %v", err)
	}
}

// This is the comparison the finalizer rests on: the Gateway's signed
// preparation against the one rebuilt from frozen inputs. Every member has to
// participate, or a mutation of it would pass unnoticed.
func TestEveryMemberParticipatesInTheComparison(t *testing.T) {
	for name, mutate := range map[string]func(*PreparedOperationV1){
		"visible statement":   func(p *PreparedOperationV1) { p.VisibleSQL += " OFFSET 1" },
		"companion statement": func(p *PreparedOperationV1) { p.CompanionSQL += " OFFSET 1" },
		"plan digest":         func(p *PreparedOperationV1) { p.PlanDigest = strings.Repeat("a", 64) },
		"catalog digest":      func(p *PreparedOperationV1) { p.CatalogDigest = strings.Repeat("b", 64) },
		"compiler identity":   func(p *PreparedOperationV1) { p.CompilerIdentity = "queryplan-v8" },
		"ordinal program":     func(p *PreparedOperationV1) { p.OrdinalProgramDigest = strings.Repeat("c", 64) },
		"dictionary set":      func(p *PreparedOperationV1) { p.DictionarySetDigest = strings.Repeat("d", 64) },
		"sidecar grants":      func(p *PreparedOperationV1) { p.SidecarGrantsDigest = strings.Repeat("e", 64) },
		"view binding":        func(p *PreparedOperationV1) { p.ViewBindingDigest = strings.Repeat("f", 64) },
		"view revision":       func(p *PreparedOperationV1) { p.ViewRegistryRevision = "r2" },
		"predicate footprint": func(p *PreparedOperationV1) { p.PredicateFootprintIdentity = strings.Repeat("9", 64) },
		"operation binding":   func(p *PreparedOperationV1) { p.PreparedOperationSHA256 = strings.Repeat("a", 64) },
		"visible target":      func(p *PreparedOperationV1) { p.PreparedVisibleTargetSHA256 = strings.Repeat("b", 64) },
		"companion target":    func(p *PreparedOperationV1) { p.PreparedCompanionTargetSHA256 = strings.Repeat("c", 64) },
		"grouped":             func(p *PreparedOperationV1) { p.Grouped = true },
		"expanded evidence":   func(p *PreparedOperationV1) { p.ExpandedEvidence = false },
		"visible fields":      func(p *PreparedOperationV1) { p.VisibleFields = []string{"other"} },
		"fact fields":         func(p *PreparedOperationV1) { p.FactFields = []string{"other"} },
		"provenance fields":   func(p *PreparedOperationV1) { p.ProvenanceFields = []string{"other"} },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := completePrepared()
			mutate(&mutated)
			if err := completePrepared().RequireSame(mutated); err == nil {
				t.Fatalf("the comparison ignored a changed %s", name)
			}
		})
	}
}

// A rejection must be reportable without carrying a statement into the message.
func TestAComparisonFailureLeaksNoSQL(t *testing.T) {
	mutated := completePrepared()
	mutated.VisibleSQL = `SELECT "salary" FROM "reporting"."payroll"`
	err := completePrepared().RequireSame(mutated)
	if err == nil {
		t.Fatal("a different visible statement compared equal")
	}
	for _, token := range []string{"SELECT", "select ", "payroll", "reporting."} {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("the rejection %q leaked statement text", err)
		}
	}
}

func TestTargetSHA256RefusesAnAbsentCompanion(t *testing.T) {
	prepared := completePrepared()
	prepared.HasCompanion, prepared.CompanionSQL = false, ""
	prepared.PreparedCompanionTargetSHA256 = ""
	if _, err := prepared.TargetSHA256(RoleCompanion); err == nil {
		t.Fatal("an absent companion produced a target binding")
	}
	if _, err := prepared.TargetSHA256("neither"); err == nil {
		t.Fatal("an unknown role produced a target binding")
	}
}
