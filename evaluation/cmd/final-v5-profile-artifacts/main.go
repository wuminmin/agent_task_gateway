// Command final-v5-profile-artifacts materializes one profile's exact snapshot
// artifact directory from a verified full artifact source.
//
// The Gateway loader requires the artifact directory to hold exactly the
// publications the active Catalog declares. A shared directory built by one
// compiler pass holds every campaign publication, so a Catalog-bound instance
// needs its own directory. This produces it as immutable regular-file copies.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	root := flag.String("root", ".", "repository root")
	registryPath := flag.String("registry", "config/profiles/registry.json", "profile registry path")
	profileID := flag.String("profile-id", "", "profile to materialize; empty materializes every eligible profile")
	source := flag.String("source", "", "verified full snapshot artifact directory")
	destination := flag.String("destination", "", "destination root for per-profile directories")
	manifestOut := flag.String("manifest-out", "", "combined manifest output path")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	if *source == "" || *destination == "" {
		fatal(errors.New("source and destination are required"))
	}
	if err := run(*root, *registryPath, *profileID, *source, *destination, *manifestOut); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "final-v5-profile-artifacts:", err)
	os.Exit(1)
}

func run(root, registryPath, profileID, source, destination, manifestOut string) error {
	value, err := os.ReadFile(filepath.Join(root, registryPath))
	if err != nil {
		return err
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(value, &registry); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	manifests := map[string]finalv5profile.ArtifactManifest{}
	for _, profile := range registry.Profiles {
		if profileID != "" && profile.ID != profileID {
			continue
		}
		// Only a profile whose Catalog the live deployment can publish has an
		// artifact set to materialize at all.
		if !profile.Status.CatalogMaterializable {
			continue
		}
		publicationSet, err := experiment.CanonicalPublicationSetSHA256(profile.Closure.Publications)
		if err != nil {
			return fmt.Errorf("profile %q: %w", profile.Alias, err)
		}
		manifest, err := finalv5profile.MaterializeProfileArtifacts(profile, registry.ContractRelease,
			source, destination, publicationSet)
		if err != nil {
			return fmt.Errorf("profile %q: %w", profile.Alias, err)
		}
		manifests[profile.ID] = manifest
		fmt.Printf("%s\t%s\t%s\t%d files\t%d bytes\n", profile.Alias, profile.ID,
			manifest.DirectorySHA256, manifest.TotalFiles, manifest.TotalBytes)
	}
	if len(manifests) == 0 {
		return errors.New("no profile was materialized")
	}
	if manifestOut == "" {
		return nil
	}
	encoded, err := json.MarshalIndent(map[string]any{"schema_version": 1,
		"record":           "taskgate-final-v5-profile-artifact-manifest-set-v1",
		"contract_release": registry.ContractRelease, "profiles": manifests}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manifestOut), 0o700); err != nil {
		return err
	}
	return os.WriteFile(manifestOut, append(encoded, '\n'), 0o600)
}
