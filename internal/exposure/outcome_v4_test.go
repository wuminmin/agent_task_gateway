package exposure

import "testing"

func TestAttachOutcomeV4RetainsFactIdentityProfiles(t *testing.T) {
	base, err := NewBaseCellFactV2("travel.expense", "snapshot-1", "row-1", "amount", "numeric", "1.00")
	if err != nil {
		t.Fatal(err)
	}
	observation, err := AttachOutcomeV4(Observation{ProfileVersion: ProfileV2, Release: []FactID{base}, Influence: []FactID{base}},
		"taskgate-query-normal-form-v2", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProfileVersion != ProfileV4 || len(observation.Outcome) != 1 {
		t.Fatalf("unexpected V4 observation: %#v", observation)
	}
	if !observation.Release[0].IsV2() || !observation.Influence[0].IsV2() || !observation.Outcome[0].IsV3() {
		t.Fatalf("V4 changed semantic FactID profiles: %#v", observation)
	}
}
