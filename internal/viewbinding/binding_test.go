package viewbinding

import (
	"strings"
	"testing"
)

func TestDigestIsProductOrderInvariantAndSemanticEvidenceSensitive(t *testing.T) {
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	c := strings.Repeat("c", 64)
	left, err := New([]ProductContract{
		{Product: "orders", CanonicalPlanDigest: a, DependencyDigest: b, InterfaceDigest: c},
		{Product: "customers", CanonicalPlanDigest: b, DependencyDigest: c, InterfaceDigest: a},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := New([]ProductContract{
		{Product: "customers", CanonicalPlanDigest: b, DependencyDigest: c, InterfaceDigest: a},
		{Product: "orders", CanonicalPlanDigest: a, DependencyDigest: b, InterfaceDigest: c},
	})
	if err != nil {
		t.Fatal(err)
	}
	leftDigest, _ := left.Digest()
	rightDigest, _ := right.Digest()
	if leftDigest != rightDigest {
		t.Fatalf("digest depends on product order: %s != %s", leftDigest, rightDigest)
	}
	right.Products[0].InterfaceDigest = strings.Repeat("d", 64)
	changed, _ := right.Digest()
	if changed == leftDigest {
		t.Fatal("interface drift did not change the task View binding")
	}
}

func TestNewRejectsDuplicateOrIncompleteContracts(t *testing.T) {
	digest := strings.Repeat("a", 64)
	contract := ProductContract{Product: "orders", CanonicalPlanDigest: digest, DependencyDigest: digest, InterfaceDigest: digest}
	if _, err := New([]ProductContract{contract, contract}); err == nil {
		t.Fatal("duplicate product was accepted")
	}
	contract.DependencyDigest = ""
	if _, err := New([]ProductContract{contract}); err == nil {
		t.Fatal("incomplete contract was accepted")
	}
}
