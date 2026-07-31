package control

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/viewbinding"
)

func TestGrantViewBindingAllowsSignedOriginalProductSupersetAfterNarrowing(t *testing.T) {
	contract := func(product, value string) viewbinding.ProductContract {
		return viewbinding.ProductContract{Product: product, CanonicalPlanDigest: strings.Repeat(value, 64),
			DependencyDigest: strings.Repeat(value, 64), InterfaceDigest: strings.Repeat(value, 64)}
	}
	set, err := viewbinding.New([]viewbinding.ProductContract{contract("kept", "a"), contract("removed", "b")})
	if err != nil {
		t.Fatalf("new binding: %v", err)
	}
	encoded, _ := json.Marshal(set)
	digest, _ := set.Digest()
	grant := TaskGrant{ApprovedProducts: []string{"kept"}, ViewBindingDigest: digest,
		ViewBindingSet: &ViewBindingSet{Digest: digest, ProfileVersion: viewbinding.Version, CanonicalJSON: encoded,
			Dependencies: []TaskViewDependency{{Product: "kept", DependencyKey: "reporting.kept"},
				{Product: "removed", DependencyKey: "reporting.removed"}}}}
	if err := validateGrantViewBinding(grant); err != nil {
		t.Fatalf("narrowed binding rejected: %v", err)
	}
	grant.ApprovedProducts = []string{"not_bound"}
	if err := validateGrantViewBinding(grant); err == nil {
		t.Fatal("approved product outside the signed original binding was accepted")
	}
}
