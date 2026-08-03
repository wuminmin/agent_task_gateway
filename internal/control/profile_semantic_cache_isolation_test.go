package control

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestSemanticCacheMissesUnderAChangedProfileBinding is the production-path
// half of the composed semantic-cache isolation proof.
//
// The formal deployment profiles have pairwise disjoint Product closures, so a
// live cross-profile execution of one query is not constructible. What can and
// must still be proven is the property that matters: a materialization
// published under one profile binding is not replayable under another. This
// exercises the real PostgreSQL-backed publish and lookup path rather than
// comparing digests, and changes one binding member at a time.
func TestSemanticCacheMissesUnderAChangedProfileBinding(t *testing.T) {
	fixture, materialization := newOrdinalMaterializationArtifactFixture(t, true)
	if _, err := fixture.store.MarkResultArtifactAvailable(context.Background(), fixture.resultID,
		"canonical-etag", "gateway"); err != nil {
		t.Fatalf("promote artifact: %v", err)
	}

	// Binding A is the one the materialization was published under.
	bindingA := fixture.lookup()
	hit, err := fixture.store.LookupOrdinalMaterialization(context.Background(), bindingA)
	if err != nil {
		t.Fatalf("same-binding lookup missed: %v", err)
	}
	if hit.CacheKeySHA256 != materialization.CacheKeySHA256 || hit.SourceQueryID != fixture.queryID {
		t.Fatalf("same-binding lookup returned %+v", hit)
	}

	// Each of these is a different deployment profile in exactly one member.
	// Every one must miss, and none may leak the source query, result, receipt
	// or root of the profile that published it.
	for name, mutate := range map[string]func(*OrdinalMaterializationLookup){
		"changed Catalog digest": func(lookup *OrdinalMaterializationLookup) {
			lookup.CatalogDigest = strings.Repeat("d", 64)
		},
		"changed grant digest": func(lookup *OrdinalMaterializationLookup) {
			lookup.GrantDigest = strings.Repeat("e", 64)
		},
		"changed dictionary set digest": func(lookup *OrdinalMaterializationLookup) {
			lookup.DictionarySetDigest = strings.Repeat("1", 64)
		},
		"changed task": func(lookup *OrdinalMaterializationLookup) {
			lookup.TaskID = fixture.taskID + "-other-profile"
		},
	} {
		lookup := bindingA
		mutate(&lookup)
		result, err := fixture.store.LookupOrdinalMaterialization(context.Background(), lookup)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s produced %+v, err=%v; want not found", name, result, err)
		}
		if result.SourceQueryID != "" || result.ResultSHA256 != "" || result.RootTaskID != "" {
			t.Fatalf("%s leaked source state: %+v", name, result)
		}
	}

	// The same complete binding still hits, so the misses above are caused by
	// the changed member and not by the row having become unusable.
	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), bindingA); err != nil {
		t.Fatalf("same-binding lookup after the negative probes: %v", err)
	}
}

// A lookup that omits any binding member must be rejected outright rather than
// silently matching on the remaining ones.
func TestSemanticCacheLookupRequiresACompleteBinding(t *testing.T) {
	fixture, _ := newOrdinalMaterializationArtifactFixture(t, true)
	for name, mutate := range map[string]func(*OrdinalMaterializationLookup){
		"no Catalog digest":        func(l *OrdinalMaterializationLookup) { l.CatalogDigest = "" },
		"no grant digest":          func(l *OrdinalMaterializationLookup) { l.GrantDigest = "" },
		"no dictionary set digest": func(l *OrdinalMaterializationLookup) { l.DictionarySetDigest = "" },
		"no task":                  func(l *OrdinalMaterializationLookup) { l.TaskID = "" },
		"no cache key":             func(l *OrdinalMaterializationLookup) { l.CacheKeySHA256 = "" },
	} {
		lookup := fixture.lookup()
		mutate(&lookup)
		if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), lookup); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s was not rejected: %v", name, err)
		}
	}
}
