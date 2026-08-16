package finalv5profile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProfileRoutingIdentityExcludesReadinessButBindsRouteAndClosure(t *testing.T) {
	registry := Registry{SchemaVersion: 1, RegistryVersion: "registry-v1", ContractRelease: "contract-v1",
		Profiles: []Profile{{ID: "profile-b", Alias: "b", CatalogPath: "config/profiles/b.catalog.yaml",
			CatalogSHA256: strings.Repeat("b", 64), Closure: Closure{SHA256: strings.Repeat("2", 64)},
			Status: ProfileStatus{ActivationSupported: false}},
			{ID: "profile-a", Alias: "a", CatalogPath: "config/profiles/a.catalog.yaml",
				CatalogSHA256: strings.Repeat("a", 64), Closure: Closure{SHA256: strings.Repeat("1", 64)}}}}
	encode := func(value Registry) []byte {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	original, err := ProfileRoutingIdentitySHA256(encode(registry))
	if err != nil {
		t.Fatal(err)
	}
	registry.Profiles[0].Status.ActivationSupported = true
	registry.Profiles[0].Status.ActivationSmokePassed = true
	registry.Profiles[0].TargetedRunEligible = true
	readinessChanged, err := ProfileRoutingIdentitySHA256(encode(registry))
	if err != nil {
		t.Fatal(err)
	}
	if readinessChanged != original {
		t.Fatal("readiness feedback changed the independent routing identity")
	}
	registry.Profiles[0].CatalogSHA256 = strings.Repeat("c", 64)
	routeChanged, err := ProfileRoutingIdentitySHA256(encode(registry))
	if err != nil {
		t.Fatal(err)
	}
	if routeChanged == original {
		t.Fatal("Catalog route mutation did not change the routing identity")
	}
	registry.Profiles[0].CatalogSHA256 = strings.Repeat("b", 64)
	registry.Profiles[0].Closure.SHA256 = strings.Repeat("3", 64)
	closureChanged, err := ProfileRoutingIdentitySHA256(encode(registry))
	if err != nil {
		t.Fatal(err)
	}
	if closureChanged == original {
		t.Fatal("closure mutation did not change the routing identity")
	}
}
