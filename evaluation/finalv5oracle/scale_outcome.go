package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	ExposureScaleOutcomeGeneratorVersion = "taskgate-final-v5-exposure-scale-outcome-candidate-v1"
	exposureScaleQueryNormalFormVersion  = "taskgate-query-normal-form-v4"
	exposureScalePublication             = "final-v5-exposure-scale-v1"
	exposureScalePublicationSHA256       = "3af007b8f7d935d8320fee8bf59352c0922dba66666f8ebd53d500cd7d5956ff"
	exposureScaleMandatoryScope          = `{"partition_key":["1"]}`

	exposureScalePredicateContextDomain  = "TASKGATE-PREDICATE-CONTEXT-V1\x00"
	exposureScaleEffectiveScopeDomain    = "TASKGATE-EFFECTIVE-MANDATORY-SCOPE-V1\x00"
	exposureScaleNormalFormDomain        = "TASKGATE-QUERY-NORMAL-FORM-V4\x00"
	exposureScaleResultObservationDomain = "TASKGATE-OUTCOME-DIGEST-V1\x00"
)

// ExposureScaleOutcomeRequest names only pre-run, source-controlled inputs.
// CatalogSHA256 is the exact complete Catalog identity that the fixed Scale
// Product will execute against; CandidateFacts must be one frozen formal
// dependency scale. No SQL or production Fact identity is accepted.
type ExposureScaleOutcomeRequest struct {
	CatalogSHA256  string
	CandidateFacts int64
	SetOptions     StreamSetOptions
}

// ExposureScaleOutcomeCandidate is the strict ordinary-set expectation for
// the Scale candidate query. Atoms contains the four exact predicate Facts;
// Composite is the result/predicate-bound fifth Fact. Members is the sorted
// five-member closure and CandidateSetSHA256 is its role-bound ordinary-set
// digest, never a production radix identity.
type ExposureScaleOutcomeCandidate struct {
	GeneratorVersion        string          `json:"generator_version"`
	ProductID               string          `json:"product_id"`
	Publication             string          `json:"publication"`
	CandidateFacts          int64           `json:"candidate_facts"`
	CandidateRows           int64           `json:"candidate_rows"`
	QueryNormalFormVersion  string          `json:"query_normal_form_version"`
	QueryNormalFormSHA256   string          `json:"query_normal_form_sha256"`
	PredicateContextSHA256  string          `json:"predicate_context_sha256"`
	ResultObservationSHA256 string          `json:"result_observation_sha256"`
	PredicateSetSHA256      string          `json:"predicate_set_sha256"`
	Atoms                   []CanonicalFact `json:"atoms"`
	Composite               CanonicalFact   `json:"composite"`
	Members                 []string        `json:"members"`
	CandidateCardinality    int64           `json:"candidate_cardinality"`
	CandidateSetSHA256      string          `json:"candidate_set_sha256"`
}

type exposureScaleOutcomePublicationBinding struct {
	SemanticProductID string `json:"semantic_product_id"`
	StableRole        string `json:"stable_role"`
	SourceNamespace   string `json:"source_namespace"`
	Snapshot          string `json:"snapshot"`
	Publication       string `json:"publication,omitempty"`
	PublicationSHA256 string `json:"publication_sha256,omitempty"`
	LineageSHA256     string `json:"lineage_sha256,omitempty"`
}

type exposureScaleOutcomePredicateRelation struct {
	Product         string `json:"product"`
	StableRole      string `json:"stable_role"`
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
}

type exposureScaleOutcomePredicateGraph struct {
	Kind      string                                  `json:"kind"`
	Relations []exposureScaleOutcomePredicateRelation `json:"relations"`
	Edges     []exposureScaleOutcomePredicateEdge     `json:"edges,omitempty"`
}

type exposureScaleOutcomePredicateEdge struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type exposureScaleOutcomePredicateContext struct {
	Version              string                                   `json:"version"`
	CatalogSHA256        string                                   `json:"catalog_sha256"`
	PublicationBundle    []exposureScaleOutcomePublicationBinding `json:"publication_bundle"`
	ViewBindingSHA256    string                                   `json:"view_binding_sha256,omitempty"`
	CanonicalFromGraph   exposureScaleOutcomePredicateGraph       `json:"canonical_from_graph"`
	EffectiveScopeSHA256 string                                   `json:"effective_scope_sha256"`
}

type exposureScaleOutcomeAggregate struct {
	Expression string `json:"expression"`
	SQLType    string `json:"sql_type"`
}

type exposureScaleOutcomeFilter struct {
	Column  string          `json:"column"`
	SQLType string          `json:"sql_type"`
	Op      string          `json:"op"`
	Value   json.RawMessage `json:"value"`
}

type exposureScaleOutcomeOrder struct {
	Expression string `json:"expression"`
	Direction  string `json:"direction"`
}

type exposureScaleOutcomeCollation struct {
	Column  string `json:"column"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// exposureScaleOutcomeNormalForm mirrors the reviewed V4 wire object. It is a
// fixed contract model, not a normalizer for caller-supplied query text.
type exposureScaleOutcomeNormalForm struct {
	Version         string                          `json:"version"`
	Profile         string                          `json:"profile"`
	SourceNamespace string                          `json:"source_namespace"`
	Snapshot        string                          `json:"snapshot"`
	LineageDigest   string                          `json:"lineage_digest,omitempty"`
	Columns         []string                        `json:"columns,omitempty"`
	Aggregates      []exposureScaleOutcomeAggregate `json:"aggregates,omitempty"`
	Filters         []exposureScaleOutcomeFilter    `json:"filters,omitempty"`
	GroupBy         []string                        `json:"group_by,omitempty"`
	OrderBy         []exposureScaleOutcomeOrder     `json:"order_by,omitempty"`
	Limit           int                             `json:"limit,omitempty"`
	Offset          int                             `json:"offset,omitempty"`
	BagSemantics    string                          `json:"bag_semantics"`
	NullLogic       string                          `json:"null_logic"`
	Collation       string                          `json:"collation"`
	Collations      []exposureScaleOutcomeCollation `json:"collations,omitempty"`
	Timezone        string                          `json:"timezone"`
	NumericMode     string                          `json:"numeric_mode"`
}

type exposureScaleOutcomeModel struct {
	CandidateFacts int64
	CatalogSHA256  string
	Publication    exposureScaleOutcomePublicationBinding
	MandatoryScope json.RawMessage
	NormalForm     exposureScaleOutcomeNormalForm
	VisibleRows    int64
	ReleaseFacts   []CanonicalFact
	Atoms          []V5PredicateAtomInput
}

// GenerateExposureScaleOutcomeCandidate independently constructs the exact
// four predicate atoms and signed composite from the frozen Scale contract.
// It performs no database access and has no input through which a production
// candidate member, set digest, prepared statement, or derived plan can enter.
func GenerateExposureScaleOutcomeCandidate(request ExposureScaleOutcomeRequest) (ExposureScaleOutcomeCandidate, error) {
	if !scaleOutcomeGeneratedDigest(request.CatalogSHA256) {
		return ExposureScaleOutcomeCandidate{}, errors.New("Scale Outcome oracle requires a non-placeholder complete Catalog SHA-256")
	}
	if !isFormalDependencyScale(request.CandidateFacts) {
		return ExposureScaleOutcomeCandidate{}, errors.New("Scale Outcome oracle requires a frozen formal candidate scale")
	}

	witnessSummaries, witness, err := SummarizeUnitWitnessSemanticSetRoles([]string{"candidate"},
		func(yield func(string) error) error {
			return StreamExposureScaleFacts(0, request.CandidateFacts, func(fact CanonicalFact) error {
				return yield(fact.SHA256)
			})
		}, request.SetOptions)
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, fmt.Errorf("derive Scale candidate witness: %w", err)
	}
	if witnessSummaries["candidate"].Cardinality != request.CandidateFacts || !validSHA256(witness) {
		return ExposureScaleOutcomeCandidate{}, errors.New("Scale Outcome oracle candidate witness violates the frozen dependency cardinality")
	}

	model, err := fixedExposureScaleOutcomeModel(request.CatalogSHA256, request.CandidateFacts, witness)
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, err
	}
	return buildExposureScaleOutcomeCandidate(model)
}

func fixedExposureScaleOutcomeModel(catalogSHA256 string, candidateFacts int64,
	witness string) (exposureScaleOutcomeModel, error) {
	if !scaleOutcomeGeneratedDigest(catalogSHA256) || !isFormalDependencyScale(candidateFacts) || !validSHA256(witness) {
		return exposureScaleOutcomeModel{}, errors.New("fixed Scale Outcome model inputs are invalid")
	}
	rows := candidateFacts / ExposureScaleFactsPerRow
	outputRowKey, err := ComposeOracleCanonicalKeyV2("group-row", "global")
	if err != nil {
		return exposureScaleOutcomeModel{}, err
	}
	releaseFact, err := BuildV2DerivedFact(V2DerivedInput{
		SnapshotBundle: []V2SnapshotBinding{{
			SourceNamespace: ExposureScaleSourceNamespace,
			Snapshot:        ExposureScaleSnapshot,
		}},
		OutputRowKey:         outputRowKey,
		NormalizedExpression: "count(*)",
		SQLType:              "bigint",
		CanonicalValue:       "i:" + strconv.FormatInt(rows, 10),
		WitnessCommitment:    witness,
	})
	if err != nil {
		return exposureScaleOutcomeModel{}, fmt.Errorf("build fixed Scale release Fact: %w", err)
	}

	filters := []exposureScaleOutcomeFilter{
		{Column: ExposureScaleSourceNamespace + ".partition_key", SQLType: "integer", Op: "=", Value: json.RawMessage(`"i:1"`)},
		{Column: ExposureScaleSourceNamespace + ".family_id", SQLType: "integer", Op: "=", Value: json.RawMessage(`"i:1"`)},
		{Column: ExposureScaleSourceNamespace + ".member_rank", SQLType: "bigint", Op: "<=", Value: json.RawMessage(strconv.Quote("i:" + strconv.FormatInt(rows, 10)))},
		{Column: ExposureScaleSourceNamespace + ".metric", SQLType: "numeric", Op: "<=", Value: json.RawMessage(`"n:1001"`)},
	}
	sort.Slice(filters, func(i, j int) bool {
		left, _ := json.Marshal(filters[i])
		right, _ := json.Marshal(filters[j])
		return bytes.Compare(left, right) < 0
	})
	publication := exposureScaleOutcomePublicationBinding{
		SemanticProductID: ExposureScaleProductID,
		StableRole:        ExposureScaleStableRole,
		SourceNamespace:   ExposureScaleSourceNamespace,
		Snapshot:          ExposureScaleSnapshot,
		Publication:       exposureScalePublication,
		PublicationSHA256: exposureScalePublicationSHA256,
	}
	return exposureScaleOutcomeModel{
		CandidateFacts: candidateFacts,
		CatalogSHA256:  catalogSHA256,
		Publication:    publication,
		MandatoryScope: json.RawMessage(exposureScaleMandatoryScope),
		NormalForm: exposureScaleOutcomeNormalForm{
			Version:         exposureScaleQueryNormalFormVersion,
			Profile:         OracleExposureProfileV5,
			SourceNamespace: ExposureScaleSourceNamespace,
			Snapshot:        ExposureScaleSnapshot,
			Aggregates: []exposureScaleOutcomeAggregate{{
				Expression: "count(*)", SQLType: "bigint",
			}},
			Filters:      filters,
			BagSemantics: "postgresql-bag",
			NullLogic:    "postgresql-three-valued",
			Collation:    "postgresql-deterministic-exact-v1",
			Timezone:     "UTC",
			NumericMode:  "postgresql-exact",
		},
		VisibleRows:  1,
		ReleaseFacts: []CanonicalFact{releaseFact},
		Atoms: []V5PredicateAtomInput{
			{SemanticProductID: ExposureScaleProductID, StableRole: ExposureScaleStableRole,
				PublicFieldID: "partition_key", SQLType: "integer", Operator: "EQ", CanonicalLiteral: "i:1"},
			{SemanticProductID: ExposureScaleProductID, StableRole: ExposureScaleStableRole,
				PublicFieldID: "family_id", SQLType: "integer", Operator: "EQ", CanonicalLiteral: "i:1"},
			{SemanticProductID: ExposureScaleProductID, StableRole: ExposureScaleStableRole,
				PublicFieldID: "member_rank", SQLType: "bigint", Operator: "LE",
				CanonicalLiteral: "i:" + strconv.FormatInt(rows, 10)},
			{SemanticProductID: ExposureScaleProductID, StableRole: ExposureScaleStableRole,
				PublicFieldID: "metric", SQLType: "numeric", Operator: "LE", CanonicalLiteral: "n:1001"},
		},
	}, nil
}

func buildExposureScaleOutcomeCandidate(model exposureScaleOutcomeModel) (ExposureScaleOutcomeCandidate, error) {
	if !isFormalDependencyScale(model.CandidateFacts) || !scaleOutcomeGeneratedDigest(model.CatalogSHA256) {
		return ExposureScaleOutcomeCandidate{}, errors.New("Scale Outcome model is not bound to a formal Scale and complete Catalog")
	}
	predicateContextSHA256, err := exposureScaleOutcomePredicateContextSHA256(model)
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, err
	}
	queryNormalFormSHA256, err := exposureScaleOutcomeNormalFormSHA256(model.NormalForm)
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, err
	}
	resultObservationSHA256, err := exposureScaleOutcomeResultObservationSHA256(model.ReleaseFacts, model.VisibleRows)
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, err
	}
	vector, err := BuildV5OutcomeVector(V5OutcomeVectorInput{
		Atoms:                   model.Atoms,
		QueryNormalFormVersion:  model.NormalForm.Version,
		QueryNormalFormSHA256:   queryNormalFormSHA256,
		ResultObservationSHA256: resultObservationSHA256,
		VisibleRows:             model.VisibleRows,
		PredicateContextSHA256:  predicateContextSHA256,
	})
	if err != nil {
		return ExposureScaleOutcomeCandidate{}, fmt.Errorf("build fixed Scale V5 Outcome vector: %w", err)
	}
	if len(vector.Atoms) != 4 || len(vector.Members) != 5 || !sort.StringsAreSorted(vector.Members) ||
		!validSHA256(vector.OutcomeSetSHA256) {
		return ExposureScaleOutcomeCandidate{}, errors.New("fixed Scale Outcome model did not produce the exact sorted five-member closure")
	}
	return ExposureScaleOutcomeCandidate{
		GeneratorVersion:        ExposureScaleOutcomeGeneratorVersion,
		ProductID:               ExposureScaleProductID,
		Publication:             exposureScalePublication,
		CandidateFacts:          model.CandidateFacts,
		CandidateRows:           model.CandidateFacts / ExposureScaleFactsPerRow,
		QueryNormalFormVersion:  model.NormalForm.Version,
		QueryNormalFormSHA256:   queryNormalFormSHA256,
		PredicateContextSHA256:  predicateContextSHA256,
		ResultObservationSHA256: resultObservationSHA256,
		PredicateSetSHA256:      vector.PredicateSetSHA256,
		Atoms:                   append([]CanonicalFact(nil), vector.Atoms...),
		Composite:               vector.Composite,
		Members:                 append([]string(nil), vector.Members...),
		CandidateCardinality:    int64(len(vector.Members)),
		CandidateSetSHA256:      vector.OutcomeSetSHA256,
	}, nil
}

func exposureScaleOutcomePredicateContextSHA256(model exposureScaleOutcomeModel) (string, error) {
	if !scaleOutcomeGeneratedDigest(model.CatalogSHA256) || !json.Valid(model.MandatoryScope) {
		return "", errors.New("Scale predicate context Catalog or mandatory scope is invalid")
	}
	effectiveScope := sha256.Sum256(append([]byte(exposureScaleEffectiveScopeDomain), model.MandatoryScope...))
	payload := exposureScaleOutcomePredicateContext{
		Version:           OraclePredicateProfileV1,
		CatalogSHA256:     model.CatalogSHA256,
		PublicationBundle: []exposureScaleOutcomePublicationBinding{model.Publication},
		CanonicalFromGraph: exposureScaleOutcomePredicateGraph{
			Kind: "scan",
			Relations: []exposureScaleOutcomePredicateRelation{{
				Product: ExposureScaleProductID, StableRole: ExposureScaleStableRole,
				SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot,
			}},
		},
		EffectiveScopeSHA256: hex.EncodeToString(effectiveScope[:]),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(exposureScalePredicateContextDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func exposureScaleOutcomeNormalFormSHA256(normal exposureScaleOutcomeNormalForm) (string, error) {
	if normal.Version != exposureScaleQueryNormalFormVersion || normal.Profile != OracleExposureProfileV5 {
		return "", errors.New("Scale Outcome normal form has an invalid V4/profile pair")
	}
	encoded, err := json.Marshal(normal)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(exposureScaleNormalFormDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func exposureScaleOutcomeResultObservationSHA256(release []CanonicalFact, visibleRows int64) (string, error) {
	if visibleRows < 0 {
		return "", errors.New("Scale Outcome visible row count is negative")
	}
	byHash := make(map[string]CanonicalFact, len(release))
	for index, fact := range release {
		if fact.Profile != OracleExposureProfileV2 {
			return "", fmt.Errorf("Scale Outcome release member %d is not a V2 Fact", index+1)
		}
		if err := ValidateCanonicalFact(fact); err != nil {
			return "", fmt.Errorf("Scale Outcome release member %d: %w", index+1, err)
		}
		if previous, exists := byHash[fact.SHA256]; exists && !bytes.Equal(previous.Payload, fact.Payload) {
			return "", errors.New("Scale Outcome release Fact SHA-256 collision")
		}
		byHash[fact.SHA256] = fact
	}
	ordered := make([]CanonicalFact, 0, len(byHash))
	for _, fact := range byHash {
		ordered = append(ordered, fact)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SHA256 < ordered[j].SHA256 })
	var payload bytes.Buffer
	_, _ = payload.WriteString(exposureScaleResultObservationDomain)
	oracleWriteCanonicalUint64(&payload, uint64(visibleRows))
	oracleWriteCanonicalUint64(&payload, uint64(len(ordered)))
	for _, fact := range ordered {
		oracleWriteCanonicalUint64(&payload, uint64(len(fact.Payload)))
		_, _ = payload.Write(fact.Payload)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func scaleOutcomeGeneratedDigest(value string) bool {
	return validSHA256(value) && strings.Trim(value, value[:1]) != ""
}
