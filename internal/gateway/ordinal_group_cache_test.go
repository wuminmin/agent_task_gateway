package gateway

import (
	"strconv"
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

func TestCanonicalGroupKeyCacheIsBoundedByVisibleRows(t *testing.T) {
	deriver := &ordinalDeriver{groupKeys: make(map[string]string)}
	for index := 0; index < 3; index++ {
		components := []string{"smallint", "visible-" + strconv.Itoa(index)}
		got, err := deriver.canonicalGroupKey(components, true)
		want, wantErr := exposure.ComposeCanonicalKeyV2("group-row", components...)
		if err != nil || wantErr != nil || got != want {
			t.Fatalf("visible group %d key = %q/%v, want %q/%v", index, got, err, want, wantErr)
		}
	}

	const nonVisibleGroups = 100_000
	var lastComponents []string
	var lastKey string
	for index := 0; index < nonVisibleGroups; index++ {
		lastComponents = []string{"bigint", "non-visible-" + strconv.Itoa(index)}
		var err error
		lastKey, err = deriver.canonicalGroupKey(lastComponents, false)
		if err != nil {
			t.Fatalf("non-visible group %d: %v", index, err)
		}
	}
	if len(deriver.groupKeys) != 3 {
		t.Fatalf("retained group cache size = %d, want visible result size 3", len(deriver.groupKeys))
	}
	if deriver.lastGroupSignature == "" || deriver.lastGroupKey != lastKey {
		t.Fatal("most-recent non-visible group was not retained in the bounded one-entry cache")
	}
	want, err := exposure.ComposeCanonicalKeyV2("group-row", lastComponents...)
	if err != nil || lastKey != want {
		t.Fatalf("last non-visible group key = %q, want %q (%v)", lastKey, want, err)
	}
	if repeated, err := deriver.canonicalGroupKey(lastComponents, false); err != nil || repeated != lastKey {
		t.Fatalf("most-recent group cache miss: %q/%v, want %q", repeated, err, lastKey)
	}
}
