package finalv5profile

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
	"regexp"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

const RQ5CatalogFamilyRecord = "taskgate-final-v5-rq5-daily-catalog-family-v1"

const PublicationSetVersion = "taskgate-final-v5-publication-set-v1"

var rq5CommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var rq5ProfileIDPattern = regexp.MustCompile(`^profile-[0-9a-f]{16}$`)

type RQ5CatalogFamilyDay struct {
	ID              string `json:"id"`
	Ordinal         int    `json:"ordinal"`
	PublicationName string `json:"publication_name"`
}

type RQ5CatalogFamilyDescriptor struct {
	SchemaVersion int                   `json:"schema_version"`
	Record        string                `json:"record"`
	ProfileAlias  string                `json:"profile_alias"`
	GeneratorPath string                `json:"generator_path"`
	ConfigPath    string                `json:"config_path"`
	WorkloadID    string                `json:"workload_id"`
	Scale         string                `json:"scale"`
	CampaignCells []string              `json:"campaign_cells"`
	Days          []RQ5CatalogFamilyDay `json:"days"`
}

type RQ5CatalogFamilyOwner struct {
	ProfileID     string
	ProfileAlias  string
	ClosureSHA256 string
	WorkloadCells []string
}

type RQ5CatalogFamilyIdentity struct {
	DescriptorSHA256    string
	SubmissionCommit    string
	GeneratorSHA256     string
	ConfigSHA256        string
	BuildManifestSHA256 string
	FamilySHA256        string
	PublicationNames    []string
}

type rq5SourceBuildManifest struct {
	SchemaVersion    int    `json:"schema_version"`
	SubmissionCommit string `json:"submission_commit"`
	BinarySHA256     string `json:"binary_sha256"`
	SourceSHA256     string `json:"source_sha256"`
	GoVersion        string `json:"go_version"`
	BuildCommand     string `json:"build_command"`
	SourceFiles      string `json:"source_files"`
}

// ResolveRQ5CatalogFamilyIdentity binds the frozen registry owner to the
// source-controlled dynamic Catalog family. The exact daily Catalog digest is
// intentionally not part of this identity: each day is reconstructed and
// checked separately from its verified publication descriptors.
func ResolveRQ5CatalogFamilyIdentity(descriptorPath, buildManifestPath,
	expectedBuildManifestSHA256 string, owner RQ5CatalogFamilyOwner) (RQ5CatalogFamilyIdentity, error) {
	var result RQ5CatalogFamilyIdentity
	descriptorBytes, err := readRQ5IdentityFile(descriptorPath, 1<<20)
	if err != nil {
		return result, fmt.Errorf("read RQ5 Catalog family descriptor: %w", err)
	}
	var descriptor RQ5CatalogFamilyDescriptor
	if err := decodeRQ5IdentityJSON(descriptorBytes, &descriptor); err != nil {
		return result, fmt.Errorf("decode RQ5 Catalog family descriptor: %w", err)
	}
	if err := validateRQ5CatalogFamilyDescriptor(descriptor, owner); err != nil {
		return result, err
	}

	manifestBytes, err := readRQ5IdentityFile(buildManifestPath, 8<<20)
	if err != nil {
		return result, fmt.Errorf("read RQ5 source build manifest: %w", err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	result.BuildManifestSHA256 = hex.EncodeToString(manifestDigest[:])
	if !digestPattern.MatchString(expectedBuildManifestSHA256) ||
		result.BuildManifestSHA256 != expectedBuildManifestSHA256 {
		return result, errors.New("RQ5 source build manifest differs from its expected digest")
	}
	var manifest rq5SourceBuildManifest
	if err := decodeRQ5IdentityJSON(manifestBytes, &manifest); err != nil {
		return result, fmt.Errorf("decode RQ5 source build manifest: %w", err)
	}
	sourceDigest := sha256.Sum256([]byte(manifest.SourceFiles))
	if manifest.SchemaVersion != 1 || !rq5CommitPattern.MatchString(manifest.SubmissionCommit) ||
		!digestPattern.MatchString(manifest.BinarySHA256) || !digestPattern.MatchString(manifest.SourceSHA256) ||
		hex.EncodeToString(sourceDigest[:]) != manifest.SourceSHA256 || strings.TrimSpace(manifest.GoVersion) == "" ||
		strings.TrimSpace(manifest.BuildCommand) == "" || manifest.SourceFiles == "" {
		return result, errors.New("RQ5 source build manifest identity is invalid")
	}
	entries, err := parseRQ5SourceInventory(manifest.SourceFiles)
	if err != nil {
		return result, err
	}
	generatorSHA, generatorOK := entries[descriptor.GeneratorPath]
	configSHA, configOK := entries[descriptor.ConfigPath]
	if !generatorOK || !configOK {
		return result, errors.New("RQ5 source build manifest omits the declared generator or config")
	}
	descriptorDigest := sha256.Sum256(descriptorBytes)
	result.DescriptorSHA256 = hex.EncodeToString(descriptorDigest[:])
	result.SubmissionCommit = manifest.SubmissionCommit
	result.GeneratorSHA256 = generatorSHA
	result.ConfigSHA256 = configSHA
	result.PublicationNames = make([]string, len(descriptor.Days))
	for index, day := range descriptor.Days {
		result.PublicationNames[index] = day.PublicationName
	}
	result.FamilySHA256 = rq5CatalogFamilyDigest(result, owner)
	return result, nil
}

func validateRQ5CatalogFamilyDescriptor(descriptor RQ5CatalogFamilyDescriptor, owner RQ5CatalogFamilyOwner) error {
	if descriptor.SchemaVersion != 1 || descriptor.Record != RQ5CatalogFamilyRecord ||
		descriptor.ProfileAlias != owner.ProfileAlias || descriptor.WorkloadID != rq5fixture.WorkloadID ||
		descriptor.Scale != rq5fixture.Scale || filepath.IsAbs(descriptor.GeneratorPath) ||
		filepath.IsAbs(descriptor.ConfigPath) || filepath.ToSlash(filepath.Clean(descriptor.GeneratorPath)) != descriptor.GeneratorPath ||
		filepath.ToSlash(filepath.Clean(descriptor.ConfigPath)) != descriptor.ConfigPath ||
		!rq5ProfileIDPattern.MatchString(owner.ProfileID) || !digestPattern.MatchString(owner.ClosureSHA256) {
		return errors.New("RQ5 Catalog family descriptor or registry owner identity is invalid")
	}
	wantCells := make([]string, 0, len(owner.WorkloadCells))
	for _, cell := range owner.WorkloadCells {
		if strings.HasPrefix(cell, "rq5/") {
			wantCells = append(wantCells, cell)
		}
	}
	gotCells := append([]string(nil), descriptor.CampaignCells...)
	sort.Strings(wantCells)
	sort.Strings(gotCells)
	if len(gotCells) != 2 || !equalRQ5Strings(gotCells, wantCells) {
		return errors.New("RQ5 Catalog family cells differ from the frozen registry assignment")
	}
	if len(descriptor.Days) != len(rq5fixture.Days) {
		return errors.New("RQ5 Catalog family does not declare the exact four-day set")
	}
	for index, day := range descriptor.Days {
		wantDay := rq5fixture.Days[index]
		wantPublication := fmt.Sprintf("daily-lineitem-%s-r%d", wantDay, rq5fixture.RowsPerPublication)
		if day.ID != wantDay || day.Ordinal != index || day.PublicationName != wantPublication {
			return fmt.Errorf("RQ5 Catalog family day %d differs from the frozen fixture", index)
		}
	}
	return nil
}

// CanonicalPublicationSetSHA256 is shared by the profile resolver and campaign
// merger so a dynamic RQ5 binding cannot acquire a second set-identity rule.
func CanonicalPublicationSetSHA256(publications []string) (string, error) {
	if len(publications) == 0 {
		return "", errors.New("a Publication set must contain at least one Publication")
	}
	sorted := append([]string(nil), publications...)
	sort.Strings(sorted)
	hash := sha256.New()
	hash.Write([]byte(PublicationSetVersion + "\x00"))
	fmt.Fprintf(hash, "%d\x00", len(sorted))
	previous := ""
	for index, name := range sorted {
		if strings.TrimSpace(name) == "" {
			return "", errors.New("a Publication set member is empty")
		}
		if index > 0 && name == previous {
			return "", fmt.Errorf("Publication %q appears twice in the set", name)
		}
		previous = name
		fmt.Fprintf(hash, "%d\x00%s\x00", len(name), name)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func parseRQ5SourceInventory(value string) (map[string]string, error) {
	entries := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		if line == "" {
			continue
		}
		digest, path, found := strings.Cut(line, "  ")
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if !found || !digestPattern.MatchString(digest) || path == "" || filepath.IsAbs(path) ||
			clean != path || clean == "." || strings.HasPrefix(clean, "../") || entries[path] != "" {
			return nil, errors.New("RQ5 source build manifest contains an unsafe or duplicate source entry")
		}
		entries[path] = digest
	}
	if len(entries) == 0 {
		return nil, errors.New("RQ5 source build manifest has an empty source inventory")
	}
	return entries, nil
}

func rq5CatalogFamilyDigest(identity RQ5CatalogFamilyIdentity, owner RQ5CatalogFamilyOwner) string {
	hash := sha256.New()
	for _, value := range []string{
		RQ5CatalogFamilyRecord, identity.DescriptorSHA256, identity.SubmissionCommit,
		identity.GeneratorSHA256, identity.ConfigSHA256, rq5fixture.FixtureSHA256(),
		owner.ProfileID, owner.ClosureSHA256,
	} {
		fmt.Fprintf(hash, "%d\x00%s\x00", len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func readRQ5IdentityFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("identity input must be a bounded regular non-symlink file")
	}
	return os.ReadFile(path)
}

func decodeRQ5IdentityJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("identity input contains a trailing JSON value")
	}
	return nil
}

func equalRQ5Strings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
