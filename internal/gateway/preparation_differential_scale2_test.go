//go:build taskgate_scale

// These cases resolve every snapshot publication the Catalog declares, which
// means the five whose rows live in the Business database (25.84 GB peak on a
// 30 GB host). The scale lane installs them; the acceptance run does not.

package gateway

import (
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// Preparation must not reach a registry, a store or a clock.
//
// The inputs are values, so a preparation that produced the same statements from
// them twice is reproducible by anyone holding them -- which is the property the
// finalizer's independent reconstruction rests on. Preparing twice from one
// input set and requiring byte equality is the cheapest statement of it.
func TestPreparationIsReproducibleFromItsInputsAlone(t *testing.T) {
	service := parityService(t, true)
	for _, test := range parityCases() {
		if _, pending := notYetExtracted[test.name]; pending {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			inputs := preparationInputsFor(t, service, test)
			first, err := physicalquery.Prepare(inputs)
			if err != nil {
				t.Fatalf("first preparation: %v", err)
			}
			second, err := physicalquery.Prepare(preparationInputsFor(t, service, test))
			if err != nil {
				t.Fatalf("second preparation: %v", err)
			}
			if err := first.Binding().RequireSame(second.Binding()); err != nil {
				t.Fatalf("two preparations from one input set disagree: %v", err)
			}
		})
	}
}
