// Package finalv5publication implements the publication-candidate boundary
// used by the final-v5 evaluation. It deliberately owns no production query
// derivation or fact identity code.
package finalv5publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	// C2ApprovalRelativePath is the only approval record accepted for P4.0-E1.
	C2ApprovalRelativePath = "evaluation/final-v5-wsl2/publication-approvals/exposure-scale-v1/approval-v1.json"
	// C2CandidateRelativePath is the exact pre-generation candidate approved by
	// Decision 20. The candidate remains immutable; generated evidence refers
	// back to these bytes instead of changing author_approved in place.
	C2CandidateRelativePath = "evaluation/final-v5-wsl2/publication-review/exposure-scale-v1/review.json"

	C2ApprovalSHA256  = "351d6bcd31a58e54df22c8bfebdc723480c7667cc6f41c8e17afd5e9264b51b3"
	C2CandidateSHA256 = "8c4fe7c322e9e2e8f1afc282487866451c2f9717d68fb1c261a8296013f973f8"

	c2ApprovalBytes  = 1254
	c2CandidateBytes = 10824
	inputMaxBytes    = 1 << 20
)

var c2CompanionNames = []string{
	"catalog.yaml",
	"compiler-input.json",
	"final-v5-exposure-scale-v1.bundle.json",
}

// FileEvidence identifies exact, reopened source-controlled bytes.
type FileEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// ApprovalEvidence is the immutable input anchor returned to the E1
// generator. CompanionFiles is sorted by path.
type ApprovalEvidence struct {
	Approval       FileEvidence                   `json:"approval"`
	Candidate      FileEvidence                   `json:"candidate"`
	CompanionFiles []FileEvidence                 `json:"companion_files"`
	CandidateState ApprovedCandidateStateEvidence `json:"approved_candidate_state"`
	ApprovalID     string                         `json:"approval_id"`
	Author         string                         `json:"author"`
	ApprovedOn     string                         `json:"approved_on"`
	DecisionPath   string                         `json:"decision_path"`
	DecisionNumber int                            `json:"decision_number"`
}

// ApprovedCandidateStateEvidence makes the anti-backfill sequence explicit in
// downstream provenance without rewriting the approved candidate bytes.
type ApprovedCandidateStateEvidence struct {
	Status          string `json:"status"`
	AuthorApproved  bool   `json:"author_approved"`
	OutcomeIdentity string `json:"outcome_identity"`
	SetAlgebra      string `json:"set_algebra"`
}

type approvalDocument struct {
	Version                string                 `json:"version"`
	ApprovalID             string                 `json:"approval_id"`
	ApprovalStatus         string                 `json:"approval_status"`
	Author                 approvalAuthor         `json:"author"`
	ApprovedCandidate      approvalCandidate      `json:"approved_candidate"`
	ApprovedCandidateState approvalCandidateState `json:"approved_candidate_state"`
	Scope                  approvalScope          `json:"scope"`
	Exclusions             approvalExclusions     `json:"exclusions"`
}

type approvalAuthor struct {
	Name             string `json:"name"`
	ApprovedOn       string `json:"approved_on"`
	DecisionDocument string `json:"decision_document"`
	DecisionNumber   int    `json:"decision_number"`
}

type approvalCandidate struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type approvalCandidateState struct {
	Version         string `json:"version"`
	Status          string `json:"status"`
	AuthorApproved  bool   `json:"author_approved"`
	OutcomeIdentity string `json:"outcome_identity"`
	SetAlgebra      string `json:"set_algebra"`
}

type approvalScope struct {
	Task          string `json:"task"`
	OutputStatus  string `json:"output_status"`
	ScaleCells    int    `json:"scale_cells"`
	ArtifactCells int    `json:"artifact_cells"`
	ProvSQLCells  int    `json:"provsql_cells"`
}

type approvalExclusions struct {
	FinalBindingExactBytes     string `json:"final_binding_exact_bytes"`
	P40E2AuthorExactByteReview string `json:"p4_0_e2_author_exact_byte_review"`
	Activation                 string `json:"activation"`
	Registry                   string `json:"registry"`
	Capability                 string `json:"capability"`
	ContractRelease            string `json:"contract_release"`
	Campaign                   string `json:"campaign"`
	Tag                        string `json:"tag"`
}

type reviewDocument struct {
	Version             string          `json:"version"`
	Status              string          `json:"status"`
	AuthorApproved      bool            `json:"author_approved"`
	OutcomeIdentity     string          `json:"outcome_identity"`
	SetAlgebra          string          `json:"set_algebra"`
	ProvSQLManifestSet  json.RawMessage `json:"provsql_manifest_set"`
	ScaleManifestSet    json.RawMessage `json:"scale_manifest_set"`
	ScaleUnion          json.RawMessage `json:"scale_union"`
	Database            json.RawMessage `json:"database"`
	Publication         json.RawMessage `json:"publication"`
	SemanticOrdinalLink json.RawMessage `json:"semantic_ordinal_link"`
	Files               []reviewFile    `json:"files"`
}

type reviewFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

var expectedC2Approval = approvalDocument{
	Version:        "taskgate-final-v5-exposure-scale-review-approval-v1",
	ApprovalID:     "APPROVE-C2",
	ApprovalStatus: "AUTHOR_APPROVED",
	Author: approvalAuthor{
		Name:             "wuminmin",
		ApprovedOn:       "2026-08-11",
		DecisionDocument: "docs/final_v5_author_decisions.md",
		DecisionNumber:   20,
	},
	ApprovedCandidate: approvalCandidate{
		Path: C2CandidateRelativePath, SHA256: C2CandidateSHA256, Bytes: c2CandidateBytes,
	},
	ApprovedCandidateState: approvalCandidateState{
		Version:         "taskgate-final-v5-exposure-scale-publication-review-v1",
		Status:          "REVIEW_CANDIDATE",
		AuthorApproved:  false,
		OutcomeIdentity: "NOT_GENERATED",
		SetAlgebra:      "NOT_GENERATED",
	},
	Scope: approvalScope{
		Task: "P4.0-E1", OutputStatus: "REVIEW_CANDIDATE",
		ScaleCells: 12, ArtifactCells: 6, ProvSQLCells: 105,
	},
	Exclusions: approvalExclusions{
		FinalBindingExactBytes:     "NOT_APPROVED",
		P40E2AuthorExactByteReview: "REQUIRED",
		Activation:                 "NOT_APPROVED",
		Registry:                   "NOT_APPROVED",
		Capability:                 "NOT_APPROVED",
		ContractRelease:            "NOT_APPROVED",
		Campaign:                   "NOT_APPROVED",
		Tag:                        "NOT_APPROVED",
	},
}

// ValidateC2Approval validates the fixed Decision-20 approval and the exact
// four-file C2 review closure below repositoryRoot. It accepts tracked 0644
// inputs but rejects group/world-writable inputs; generated private outputs
// have the stricter mode-0600 contract implemented in io.go.
func ValidateC2Approval(repositoryRoot string) (ApprovalEvidence, error) {
	var evidence ApprovalEvidence
	root, err := cleanRepositoryRoot(repositoryRoot)
	if err != nil {
		return evidence, err
	}
	approvalPath, err := fixedRepositoryPath(root, C2ApprovalRelativePath)
	if err != nil {
		return evidence, err
	}
	approvalBytes, approvalInfo, err := readSafeInput(approvalPath, "C2 approval record", inputMaxBytes)
	if err != nil {
		return evidence, err
	}
	var approval approvalDocument
	if err := strictJSON(approvalBytes, &approval); err != nil {
		return evidence, fmt.Errorf("decode C2 approval record: %w", err)
	}
	if !reflect.DeepEqual(approval, expectedC2Approval) {
		return evidence, errors.New("C2 approval record differs from exact Decision 20 authority, scope, or exclusions")
	}
	if approvalInfo.Size() != c2ApprovalBytes || sha256Hex(approvalBytes) != C2ApprovalSHA256 {
		return evidence, errors.New("C2 approval record bytes differ from the committed approval anchor")
	}

	candidatePath, err := fixedRepositoryPath(root, C2CandidateRelativePath)
	if err != nil {
		return evidence, err
	}
	candidateDirectory := filepath.Dir(candidatePath)
	if err := requireSafeDirectory(candidateDirectory, "C2 review directory"); err != nil {
		return evidence, err
	}
	entries, err := os.ReadDir(candidateDirectory)
	if err != nil {
		return evidence, errors.New("read C2 review directory")
	}
	wantNames := append([]string{"review.json"}, c2CompanionNames...)
	sort.Strings(wantNames)
	gotNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	sort.Strings(gotNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		return evidence, errors.New("C2 review directory is not the exact four-file closure")
	}

	candidateBytes, candidateInfo, err := readSafeInput(candidatePath, "C2 approved candidate", inputMaxBytes)
	if err != nil {
		return evidence, err
	}
	var review reviewDocument
	if err := strictJSON(candidateBytes, &review); err != nil {
		return evidence, fmt.Errorf("decode C2 approved candidate: %w", err)
	}
	if candidateInfo.Size() != approval.ApprovedCandidate.Bytes ||
		candidateInfo.Size() != c2CandidateBytes ||
		sha256Hex(candidateBytes) != approval.ApprovedCandidate.SHA256 ||
		sha256Hex(candidateBytes) != C2CandidateSHA256 {
		return evidence, errors.New("C2 approved candidate bytes differ from Decision 20")
	}
	if review.Version != approval.ApprovedCandidateState.Version ||
		review.Status != approval.ApprovedCandidateState.Status ||
		review.AuthorApproved != approval.ApprovedCandidateState.AuthorApproved ||
		review.OutcomeIdentity != approval.ApprovedCandidateState.OutcomeIdentity ||
		review.SetAlgebra != approval.ApprovedCandidateState.SetAlgebra {
		return evidence, errors.New("C2 approved candidate no longer has its approved pre-generation state")
	}

	descriptors := make(map[string]reviewFile, len(review.Files))
	for _, descriptor := range review.Files {
		if _, duplicate := descriptors[descriptor.Name]; duplicate {
			return evidence, errors.New("C2 approved candidate repeats a companion descriptor")
		}
		descriptors[descriptor.Name] = descriptor
	}
	if len(descriptors) != len(c2CompanionNames) {
		return evidence, errors.New("C2 approved candidate does not bind exactly three companions")
	}
	for _, name := range c2CompanionNames {
		descriptor, found := descriptors[name]
		if !found || descriptor.Bytes <= 0 || descriptor.Bytes > inputMaxBytes || !validSHA256(descriptor.SHA256) {
			return evidence, errors.New("C2 approved candidate companion descriptor set is invalid")
		}
		path, pathErr := fixedRepositoryPath(root, filepath.ToSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), name)))
		if pathErr != nil {
			return evidence, pathErr
		}
		value, info, readErr := readSafeInput(path, "C2 review companion", inputMaxBytes)
		if readErr != nil {
			return evidence, readErr
		}
		if info.Size() != descriptor.Bytes || sha256Hex(value) != descriptor.SHA256 {
			return evidence, fmt.Errorf("C2 review companion %q differs from review.json", name)
		}
		evidence.CompanionFiles = append(evidence.CompanionFiles, FileEvidence{
			Path:   filepath.ToSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), name)),
			SHA256: descriptor.SHA256,
			Bytes:  descriptor.Bytes,
		})
	}
	sort.Slice(evidence.CompanionFiles, func(i, j int) bool {
		return evidence.CompanionFiles[i].Path < evidence.CompanionFiles[j].Path
	})
	evidence.Approval = FileEvidence{Path: C2ApprovalRelativePath, SHA256: C2ApprovalSHA256, Bytes: c2ApprovalBytes}
	evidence.Candidate = FileEvidence{Path: C2CandidateRelativePath, SHA256: C2CandidateSHA256, Bytes: c2CandidateBytes}
	evidence.CandidateState = ApprovedCandidateStateEvidence{Status: review.Status,
		AuthorApproved: review.AuthorApproved, OutcomeIdentity: review.OutcomeIdentity, SetAlgebra: review.SetAlgebra}
	evidence.ApprovalID = approval.ApprovalID
	evidence.Author = approval.Author.Name
	evidence.ApprovedOn = approval.Author.ApprovedOn
	evidence.DecisionPath = approval.Author.DecisionDocument
	evidence.DecisionNumber = approval.Author.DecisionNumber
	return evidence, nil
}

func cleanRepositoryRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("repository root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("resolve repository root")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("repository root must be a non-symlink directory")
	}
	return filepath.Clean(abs), nil
}

func fixedRepositoryPath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.HasPrefix(relative, "../") {
		return "", errors.New("source-controlled path is not a clean repository-relative path")
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for index, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("source-controlled path is invalid")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("source-controlled directory component %d is absent, non-directory, or a symlink", index)
		}
	}
	return filepath.Join(root, filepath.FromSlash(relative)), nil
}

func requireSafeDirectory(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be an owner-controlled non-symlink directory", label)
	}
	return nil
}

func readSafeInput(path, label string, limit int64) ([]byte, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Mode().Perm()&0o022 != 0 || before.Size() <= 0 || before.Size() > limit {
		return nil, nil, fmt.Errorf("%s must be a bounded non-writable regular non-symlink file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s", label)
	}
	value, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil || int64(len(value)) > limit ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
		!after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || after.Mode().Perm()&0o022 != 0 ||
		int64(len(value)) != after.Size() {
		return nil, nil, fmt.Errorf("%s changed or became unsafe while being read", label)
	}
	return value, after, nil
}

func strictJSON(value []byte, target any) error {
	if len(bytes.TrimSpace(value)) == 0 {
		return errors.New("empty JSON document")
	}
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON object key %q", key)
				}
				seen[key] = true
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := consume(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
