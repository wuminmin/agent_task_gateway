package finalv5publication

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const (
	baseCatalogRelativePath  = "config/catalog.yaml"
	oracleRootRelativePath   = "evaluation/final-v5-wsl2/oracle-manifests"
	contractRootRelativePath = "evaluation/final-v5-wsl2"
)

type contractInputEvidence struct {
	Release   string                     `json:"release"`
	Index     FileEvidence               `json:"index"`
	Artifacts []contractArtifactEvidence `json:"artifacts"`
}

type contractArtifactEvidence struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type generationMaterials struct {
	approval             ApprovalEvidence
	decision             FileEvidence
	baseCatalog          FileEvidence
	baseCatalogBytes     []byte
	approvedScaleCatalog FileEvidence
	approvedScaleBytes   []byte
	runtime              *finalv5contracts.Runtime
	contract             contractInputEvidence
	scaleManifests       []finalv5oracle.ExposureScaleManifestArtifact
	provSQLManifests     []finalv5oracle.ProvSQLManifestArtifact
}

func loadGenerationMaterials(repositoryRoot string) (generationMaterials, error) {
	var result generationMaterials
	root, err := cleanRepositoryRoot(repositoryRoot)
	if err != nil {
		return result, err
	}
	// Approval validation deliberately precedes every other input load and any
	// database connection. It proves the generation sequence cannot be backfilled.
	result.approval, err = ValidateC2Approval(root)
	if err != nil {
		return result, err
	}
	result.decision, _, err = readRepositoryEvidence(root, result.approval.DecisionPath, "Decision 29 document")
	if err != nil {
		return result, err
	}
	result.baseCatalog, result.baseCatalogBytes, err = readRepositoryEvidence(root, baseCatalogRelativePath, "base Catalog")
	if err != nil {
		return result, err
	}
	scaleCatalogPath := filepath.ToSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), "catalog.yaml"))
	result.approvedScaleCatalog, result.approvedScaleBytes, err = readRepositoryEvidence(root, scaleCatalogPath,
		"C2-approved Scale Catalog companion")
	if err != nil {
		return result, err
	}
	if result.approvedScaleCatalog.SHA256 != approvedC2ScaleCatalogSHA256 ||
		!containsFileEvidence(result.approval.CompanionFiles, result.approvedScaleCatalog) {
		return result, errors.New("C2-approved Scale Catalog companion is not anchored by approval evidence")
	}

	result.runtime, err = finalv5contracts.LoadRuntime()
	if err != nil {
		return result, fmt.Errorf("load verified Final-V5 Contract runtime: %w", err)
	}
	indexed, err := result.runtime.IndexedArtifacts()
	if err != nil {
		return result, err
	}
	indexPath := path.Join(contractRootRelativePath, "contracts/index-v1.json")
	indexEvidence, _, err := readRepositoryEvidence(root, indexPath, "Contract Index")
	if err != nil || indexEvidence.SHA256 != result.runtime.IndexSHA256() {
		return result, errors.New("source-controlled Contract Index differs from embedded Runtime")
	}
	result.contract = contractInputEvidence{Release: result.runtime.ContractRelease(),
		Index: indexEvidence, Artifacts: make([]contractArtifactEvidence, 0, len(indexed))}
	for _, artifact := range indexed {
		relative := path.Join(contractRootRelativePath, artifact.Path)
		evidence, _, readErr := readRepositoryEvidence(root, relative, "Contract Index artifact")
		if readErr != nil {
			return result, readErr
		}
		if evidence.SHA256 != artifact.SHA256 {
			return result, fmt.Errorf("Contract Index artifact %s differs from embedded Runtime", artifact.Path)
		}
		result.contract.Artifacts = append(result.contract.Artifacts, contractArtifactEvidence{Kind: artifact.Kind,
			Path: evidence.Path, SHA256: evidence.SHA256, Bytes: evidence.Bytes})
	}
	result.scaleManifests, err = loadScaleManifestClosure(root)
	if err != nil {
		return result, err
	}
	result.provSQLManifests, err = loadProvSQLManifestClosure(root)
	if err != nil {
		return result, err
	}
	return result, nil
}

func readRepositoryEvidence(root, relative, label string) (FileEvidence, []byte, error) {
	path, err := fixedRepositoryPath(root, relative)
	if err != nil {
		return FileEvidence{}, nil, err
	}
	value, info, err := readSafeInput(path, label, inputMaxBytes)
	if err != nil {
		return FileEvidence{}, nil, err
	}
	return FileEvidence{Path: relative, SHA256: sha256Hex(value), Bytes: info.Size()}, value, nil
}

func loadScaleManifestClosure(root string) ([]finalv5oracle.ExposureScaleManifestArtifact, error) {
	wanted := make([]string, 0, 24)
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		for _, mode := range []string{finalv5oracle.ExposureScaleModeNovel,
			finalv5oracle.ExposureScaleModeSemanticReplay} {
			relative, err := finalv5oracle.ExposureScaleDependencyManifestPath(cell.Scale, mode)
			if err != nil {
				return nil, err
			}
			wanted = append(wanted, relative)
		}
	}
	if err := requireManifestDirectoryClosure(root, "scale", wanted); err != nil {
		return nil, err
	}
	result := make([]finalv5oracle.ExposureScaleManifestArtifact, 0, len(wanted))
	for _, relative := range wanted {
		repositoryPath := path.Join(oracleRootRelativePath, relative)
		evidence, value, err := readRepositoryEvidence(root, repositoryPath, "Scale oracle manifest")
		if err != nil {
			return nil, err
		}
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			return nil, fmt.Errorf("decode Scale oracle manifest %s: %w", relative, err)
		}
		result = append(result, finalv5oracle.ExposureScaleManifestArtifact{RelativePath: relative,
			SHA256: evidence.SHA256, Manifest: manifest})
	}
	return result, nil
}

func loadProvSQLManifestClosure(root string) ([]finalv5oracle.ProvSQLManifestArtifact, error) {
	wanted := make([]string, 0, 105)
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		relative, err := finalv5oracle.ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, relative)
	}
	if err := requireManifestDirectoryClosure(root, "provsql", wanted); err != nil {
		return nil, err
	}
	result := make([]finalv5oracle.ProvSQLManifestArtifact, 0, len(wanted))
	for _, relative := range wanted {
		repositoryPath := path.Join(oracleRootRelativePath, relative)
		evidence, value, err := readRepositoryEvidence(root, repositoryPath, "ProvSQL oracle manifest")
		if err != nil {
			return nil, err
		}
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			return nil, fmt.Errorf("decode ProvSQL oracle manifest %s: %w", relative, err)
		}
		result = append(result, finalv5oracle.ProvSQLManifestArtifact{RelativePath: relative,
			SHA256: evidence.SHA256, Manifest: manifest})
	}
	return result, nil
}

func requireManifestDirectoryClosure(root, subtree string, wanted []string) error {
	directoryRelative := path.Join(oracleRootRelativePath, subtree)
	directory, err := fixedRepositoryPath(root, directoryRelative+"/sentinel")
	if err != nil {
		return err
	}
	directory = filepath.Dir(directory)
	if err := requireSafeDirectory(directory, "oracle manifest subtree"); err != nil {
		return err
	}
	wantedRelative := make([]string, len(wanted))
	for index, relative := range wanted {
		if !strings.HasPrefix(relative, subtree+"/") {
			return errors.New("oracle manifest expected path escapes its fixed subtree")
		}
		wantedRelative[index] = strings.TrimPrefix(relative, subtree+"/")
	}
	sort.Strings(wantedRelative)
	actual := make([]string, 0, len(wanted))
	err = filepath.WalkDir(directory, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == directory {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return errors.New("oracle manifest subtree contains an unsafe entry")
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("oracle manifest subtree contains a non-regular entry")
		}
		relative, relErr := filepath.Rel(directory, current)
		if relErr != nil {
			return errors.New("resolve oracle manifest subtree member")
		}
		actual = append(actual, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, wantedRelative) {
		return fmt.Errorf("%s oracle manifest subtree is not the exact %d-file closure", subtree, len(wanted))
	}
	return nil
}

func containsFileEvidence(values []FileEvidence, wanted FileEvidence) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
