package semanticcache

import (
	"strings"
	"testing"
)

func TestBindingDigestIsStableAndAuthorityScoped(t *testing.T) {
	binding := validBinding()
	first, err := binding.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	second, err := binding.Digest()
	if err != nil || first != second {
		t.Fatalf("stable digest = %q, %v; want %q", second, err, first)
	}
	for name, mutate := range map[string]func(*Binding){
		"task":       func(value *Binding) { value.TaskID = "task-child" },
		"grant":      func(value *Binding) { value.GrantDigest = strings.Repeat("b", 64) },
		"scope":      func(value *Binding) { value.AuthorizationDigest = strings.Repeat("c", 64) },
		"parameter":  func(value *Binding) { value.TypedNormalForm += "\x1fparameter=2" },
		"catalog":    func(value *Binding) { value.CatalogDigest = strings.Repeat("d", 64) },
		"schema":     func(value *Binding) { value.SchemaDigest = strings.Repeat("e", 64) },
		"dictionary": func(value *Binding) { value.DictionarySetDigest = strings.Repeat("f", 64) },
		"compiler":   func(value *Binding) { value.CompilerVersion = "taskgate-ordinal-compiler-v2" },
		"ordering":   func(value *Binding) { value.OrderingVersion = "taskgate-canonical-order-v2" },
		"pagination": func(value *Binding) { value.PaginationVersion = "taskgate-pagination-v2" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := binding
			mutate(&changed)
			digest, digestErr := changed.Digest()
			if digestErr != nil {
				t.Fatalf("changed Digest: %v", digestErr)
			}
			if digest == first {
				t.Fatalf("%s change did not partition cache", name)
			}
		})
	}
}

func TestBindingRejectsMissingSnapshotOrAuthorityDigests(t *testing.T) {
	binding := validBinding()
	binding.DictionarySetDigest = ""
	if _, err := binding.Digest(); err == nil {
		t.Fatal("missing dictionary digest was accepted")
	}
	binding = validBinding()
	binding.AuthorizationDigest = strings.Repeat("A", 64)
	if _, err := binding.Digest(); err == nil {
		t.Fatal("non-canonical authorization digest was accepted")
	}
}

func validBinding() Binding {
	return Binding{
		TaskID: "task-root", GrantDigest: strings.Repeat("1", 64),
		AuthorizationDigest: strings.Repeat("2", 64),
		TypedNormalForm:     "taskgate-plan-nf-v2\x1fparameter=1",
		PlanDigest:          strings.Repeat("3", 64), CatalogDigest: strings.Repeat("4", 64),
		SchemaDigest: strings.Repeat("5", 64), DictionarySetDigest: strings.Repeat("6", 64),
		ExposureProfile: "taskgate-exposure-v4",
	}
}
