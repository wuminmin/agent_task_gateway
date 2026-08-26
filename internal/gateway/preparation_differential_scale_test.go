//go:build taskgate_scale

// This case prepares an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so it belongs on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"errors"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// Every extracted shape must prepare identically in both implementations.
func TestExtractedPreparationMatchesTheGateway(t *testing.T) {
	for _, test := range parityCases() {
		t.Run(test.name, func(t *testing.T) {
			service, production := prepareParityCase(t, test)
			resolved := resolveParityCase(t, service, test)

			inputs := preparationInputsFor(t, service, test)
			prepared, err := physicalquery.Prepare(inputs)
			if _, pending := notYetExtracted[test.name]; pending {
				if err == nil {
					t.Fatalf("shape %q prepared, but it is listed as not yet extracted; "+
						"remove it from notYetExtracted so its parity is verified", test.name)
				}
				if !errors.Is(err, physicalquery.ErrShapeNotExtracted) {
					t.Fatalf("shape %q was refused for a reason other than being unextracted: %v",
						test.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("shape %q is not listed as pending but Prepare refused it: %v", test.name, err)
			}

			// The prepared object must also be the one its binding describes, and
			// must be provably prepared from these inputs. A parity pass over an
			// object that failed either check would be comparing the right bytes
			// carried by the wrong evidence.
			compiler, err := physicalquery.LocalCompilerIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.RequireInputs(inputs, compiler); err != nil {
				t.Fatalf("the prepared operation does not bind these inputs: %v", err)
			}

			extracted := extractedShapeOf(t, prepared, resolved)
			requireSameShape(t, production, extracted)
		})
	}
}
