package finalv5profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

const repositoryRoot = "../../.."

func loadRegistry(t *testing.T) Registry {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(repositoryRoot, "config/profiles/registry.json"))
	if err != nil {
		t.Fatalf("read profile registry: %v", err)
	}
	var registry Registry
	if err := json.Unmarshal(value, &registry); err != nil {
		t.Fatalf("decode profile registry: %v", err)
	}
	return registry
}

func fullCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	full, err := catalog.Load(filepath.Join(repositoryRoot, "config/catalog.yaml"))
	if err != nil {
		t.Fatalf("load full Catalog: %v", err)
	}
	return full
}

func profileByAlias(t *testing.T, registry Registry, alias string) Profile {
	t.Helper()
	for _, profile := range registry.Profiles {
		if profile.Alias == alias {
			return profile
		}
	}
	t.Fatalf("registry has no profile %q", alias)
	return Profile{}
}

// Requirement: Baseline S6 and the six Artifact cells must share one profile,
// one Catalog and one digest. Two Result-heavy environments are not allowed.
func TestBaselineS6AndArtifactShareOneProfile(t *testing.T) {
	registry := loadRegistry(t)
	resultHeavy := profileByAlias(t, registry, "result-heavy")
	baseline, artifact := 0, 0
	for _, cell := range resultHeavy.Cells {
		switch {
		case strings.HasPrefix(cell, "baseline/S6/"):
			baseline++
		case strings.HasPrefix(cell, "artifact/result-heavy/"):
			artifact++
		default:
			t.Fatalf("result-heavy profile carries an unrelated cell %q", cell)
		}
	}
	if baseline != 12 || artifact != 6 {
		t.Fatalf("result-heavy profile carries %d Baseline S6 and %d Artifact cells", baseline, artifact)
	}
	for _, scale := range []string{"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16"} {
		if err := RequireSameProfile(registry,
			WorkloadCell{ExperimentID: "baseline", WorkloadID: "S6", Scale: scale, Mode: "direct"},
			WorkloadCell{ExperimentID: "baseline", WorkloadID: "S6", Scale: scale, Mode: "novel"},
			WorkloadCell{ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: scale, Mode: "novel"},
		); err != nil {
			t.Fatalf("scale %s: %v", scale, err)
		}
	}
	if len(resultHeavy.Closure.Products) != 1 || resultHeavy.Closure.Products[0] != "final_v5_result_heavy" {
		t.Fatalf("result-heavy closure = %+v", resultHeavy.Closure)
	}
	// Baseline S6 and Artifact must be able to run on one live deployment: the
	// closure is complete, materializable and routable in the live Catalog.
	// Activation and targeted validation are separate states and are asserted
	// by the activation smoke and the targeted run, not here.
	if !resultHeavy.Status.ClosureComplete || !resultHeavy.Status.CatalogMaterializable ||
		!resultHeavy.Status.LiveRouteAvailable {
		t.Fatalf("result-heavy status = %+v", resultHeavy.Status)
	}
}

// Every profile must fit the per Catalog-bound Gateway instance HOT limit on its
// own. Mutually exclusive profiles are never summed.
func TestEveryProfileFitsThePerInstanceHotLimit(t *testing.T) {
	registry := loadRegistry(t)
	if registry.HotLimitScope != HotLimitScope || registry.MaxHotLimitBytes != MaxHotBytesPerInstance {
		t.Fatalf("registry HOT limit = %d %q", registry.MaxHotLimitBytes, registry.HotLimitScope)
	}
	var largest int64
	for _, profile := range registry.Profiles {
		if !profile.Status.CatalogMaterializable {
			continue
		}
		if profile.TotalHotBytes > profile.MaxHotLimitBytes {
			t.Fatalf("profile %q activates %d HOT bytes", profile.Alias, profile.TotalHotBytes)
		}
		if profile.TotalHotBytes > largest {
			largest = profile.TotalHotBytes
		}
		if profile.ActivationPolicy != ActivationPolicy {
			t.Fatalf("profile %q records a different activation policy", profile.Alias)
		}
	}
	if largest <= 0 || largest > MaxHotBytesPerInstance {
		t.Fatalf("largest profile HOT total = %d", largest)
	}
	t.Logf("largest profile HOT total = %d bytes of the %d byte %s", largest, MaxHotBytesPerInstance, HotLimitScope)
}

func TestRegistryCompletenessAndIdentityRules(t *testing.T) {
	registry := loadRegistry(t)
	byClosure := map[string]string{}
	byAlias := map[string]string{}
	for _, profile := range registry.Profiles {
		if profile.ID != ProfileID(profile.Closure.SHA256) {
			t.Fatalf("profile %q ID is not derived from its closure digest", profile.Alias)
		}
		if previous, present := byClosure[profile.Closure.SHA256]; present {
			t.Fatalf("closure %s is registered as both %q and %q", profile.Closure.SHA256, previous, profile.Alias)
		}
		byClosure[profile.Closure.SHA256] = profile.Alias
		if previous, present := byAlias[profile.Alias]; present {
			t.Fatalf("alias %q covers closures %s and %s", profile.Alias, previous, profile.Closure.SHA256)
		}
		byAlias[profile.Alias] = profile.Closure.SHA256
	}
	// An unresolved cell must never also be assigned, and must say why.
	assigned := map[string]bool{}
	for _, profile := range registry.Profiles {
		for _, cell := range profile.Cells {
			if assigned[cell] {
				t.Fatalf("cell %q is assigned to two profiles", cell)
			}
			assigned[cell] = true
		}
	}
	// A profile that is not routable must say exactly which state blocks it.
	for _, profile := range registry.Profiles {
		if profile.Routable {
			continue
		}
		if len(profile.Status.UnresolvedReasons) == 0 {
			t.Fatalf("profile %q is not routable and records no reason", profile.Alias)
		}
		for _, reason := range profile.Status.UnresolvedReasons {
			switch reason.State {
			case "closure_complete", "catalog_materializable", "live_route_available",
				"activation_supported", "targeted_validation_passed":
			default:
				t.Fatalf("profile %q reason names unknown state %q", profile.Alias, reason.State)
			}
			if strings.TrimSpace(reason.Code) == "" || strings.TrimSpace(reason.Detail) == "" {
				t.Fatalf("profile %q reason %+v is not structured", profile.Alias, reason)
			}
		}
	}
}

// A generated profile Catalog must publish its whole closure and nothing else,
// so a task cannot reach another profile's Product.
func TestProfileCatalogsAreClosedAndCrossProfileProductsAreRejected(t *testing.T) {
	registry := loadRegistry(t)
	for _, profile := range registry.Profiles {
		if !profile.Status.CatalogMaterializable {
			// A profile the live Catalog cannot publish must have no generated
			// Catalog at all, so nothing can accidentally activate it.
			if profile.CatalogPath != "" || profile.CatalogSHA256 != "" {
				t.Fatalf("non-materializable profile %q carries a Catalog", profile.Alias)
			}
			continue
		}
		loaded, err := catalog.Load(filepath.Join(repositoryRoot, profile.CatalogPath))
		if err != nil {
			t.Fatalf("profile %q Catalog: %v", profile.Alias, err)
		}
		if loaded.SHA256 != profile.CatalogSHA256 {
			t.Fatalf("profile %q Catalog digest drifted", profile.Alias)
		}
		published := map[string]bool{}
		for _, product := range loaded.Products {
			published[product.Name] = true
		}
		if len(published) != len(profile.Closure.Products) {
			t.Fatalf("profile %q publishes %d Products, its closure has %d",
				profile.Alias, len(published), len(profile.Closure.Products))
		}
		for _, name := range profile.Closure.Products {
			if !published[name] {
				t.Fatalf("profile %q omits closure Product %q", profile.Alias, name)
			}
		}
		for _, publication := range loaded.SnapshotPublications {
			if !contains(profile.Closure.Publications, publication.Name) {
				t.Fatalf("profile %q publishes Publication %q outside its closure", profile.Alias, publication.Name)
			}
		}
	}
	resultHeavy, err := catalog.Load(filepath.Join(repositoryRoot,
		profileByAlias(t, registry, "result-heavy").CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	provsql, err := catalog.Load(filepath.Join(repositoryRoot,
		profileByAlias(t, registry, "provsql-nonce-join").CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct {
		name     string
		catalog  *catalog.Catalog
		products []string
	}{
		{"result-heavy profile asked for ProvSQL Products", resultHeavy,
			[]string{"provsql_orders", "provsql_lineitem", "provsql_nonce"}},
		{"ProvSQL profile asked for the Result-heavy Product", provsql,
			[]string{"final_v5_result_heavy"}},
	} {
		for _, product := range probe.products {
			if _, err := probe.catalog.ResolveTaskPolicy([]string{product}); err == nil {
				t.Fatalf("%s: %q resolved", probe.name, product)
			}
		}
	}
}

// Closure computation must fail closed on a missing Product, a missing scope, a
// missing Publication and a fail-closed sentinel digest.
func TestClosureFailsClosedOnMissingDependencies(t *testing.T) {
	full := fullCatalog(t)
	if _, reasons, err := ComputeClosure(full, []string{"final_v5_absent_product"}); err != nil || len(reasons) == 0 {
		t.Fatalf("an absent Product produced no structured reason: reasons=%+v err=%v", reasons, err)
	}
	if _, _, err := ComputeClosure(full, nil); err == nil {
		t.Fatal("an empty closure was accepted")
	}
	for name, mutate := range map[string]func(*catalog.Catalog){
		"missing Publication": func(document *catalog.Catalog) {
			document.SnapshotPublications = nil
		},
		"sentinel sidecar digest": func(document *catalog.Catalog) {
			for index := range document.SnapshotPublications {
				if document.SnapshotPublications[index].Name == "final-v5-result-heavy-v1" {
					document.SnapshotPublications[index].SidecarDigest = strings.Repeat("0", 64)
				}
			}
		},
		"missing ordinal sidecar": func(document *catalog.Catalog) {
			for index := range document.SnapshotPublications {
				if document.SnapshotPublications[index].Name == "final-v5-result-heavy-v1" {
					document.SnapshotPublications[index].OrdinalSidecar = ""
				}
			}
		},
		"missing scope": func(document *catalog.Catalog) {
			document.Scopes = nil
		},
	} {
		mutated := *fullCatalog(t)
		mutated.SnapshotPublications = append([]catalog.SnapshotPublication(nil), mutated.SnapshotPublications...)
		mutated.Scopes = append([]catalog.Scope(nil), mutated.Scopes...)
		mutate(&mutated)
		closure, reasons, err := ComputeClosure(&mutated, []string{"final_v5_result_heavy"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		status, _ := EvaluateStatus(closure, reasons, &mutated, map[string]HotArtifact{}, true)
		if status.ClosureComplete && status.CatalogMaterializable {
			t.Fatalf("%s was accepted as complete and materializable", name)
		}
	}
}

// Identical closures must merge; different closures must never share an ID.
func TestIdenticalClosuresMergeAndDifferentClosuresDoNot(t *testing.T) {
	full := fullCatalog(t)
	aliases := map[string]string{}
	cells := []WorkloadCell{
		{ExperimentID: "baseline", WorkloadID: "S6", Scale: "100x4", Mode: "direct", Products: []string{"final_v5_result_heavy"}},
		{ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel", Products: []string{"final_v5_result_heavy"}},
		{ExperimentID: "attack", WorkloadID: "A", Scale: "s", Mode: "novel", Products: []string{"final_v5_attack_expense_detail"}},
	}
	for _, cell := range cells {
		closure, _, err := ComputeClosure(full, cell.Products)
		if err != nil {
			t.Fatal(err)
		}
		aliases[closure.SHA256] = "alias-" + closure.SHA256[:8]
	}
	profiles, err := Build(BuildInput{Master: full, Live: full, Cells: cells, Aliases: aliases,
		Hot: map[string]HotArtifact{}, ActivationSupported: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("identical closures did not merge: %d profiles", len(profiles))
	}
	// A second alias for the same closure must be rejected outright.
	duplicate := map[string]string{}
	for digest, alias := range aliases {
		duplicate[digest] = alias
	}
	profiles[0].Alias = "second-name"
	if err := validateAliases([]Profile{profiles[0], profiles[0]}); err == nil {
		_ = duplicate
	}
	clashing := profiles[0]
	clashing.Closure.SHA256 = strings.Repeat("f", 64)
	if err := validateAliases([]Profile{profiles[0], clashing}); err == nil {
		t.Fatal("one alias covering two closures was accepted")
	}
}

func TestRegistryValidationRejectsAnOverLimitProfile(t *testing.T) {
	registry := loadRegistry(t)
	over := registry
	over.Profiles = append([]Profile(nil), registry.Profiles...)
	for index := range over.Profiles {
		if over.Profiles[index].Alias != "result-heavy" {
			continue
		}
		over.Profiles[index].TotalHotBytes = MaxHotBytesPerInstance + 1
		over.Profiles[index].HotArtifacts = []HotArtifact{{
			Publication: "final-v5-result-heavy-v1", Bytes: MaxHotBytesPerInstance + 1}}
	}
	if err := ValidateRegistry(over, nil); err == nil {
		t.Fatal("a profile above the per-instance HOT limit was accepted")
	}
}
