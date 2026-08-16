// Command final-v5-campaign-plan derives the per-profile deployment plan for a
// formal campaign replicate.
//
// The formal runner brings one deployment up and runs nine experiments against
// it. That cannot serve the Artifact arm, which verifies that the Gateway
// signing its Receipts was serving the result-heavy profile Catalog, so the
// campaign becomes one deployment per profile. This command says which
// deployments those are, which cells each carries, and which profiles are not
// yet activatable -- it never activates or measures anything.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	root := flag.String("root", ".", "repository root")
	registryPath := flag.String("registry", "config/profiles/registry.json", "profile registry path")
	requireReady := flag.Bool("require-ready", false,
		"exit non-zero unless every planned deployment is activatable today")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "positional arguments are not accepted")
		os.Exit(2)
	}
	if err := run(*root, *registryPath, *requireReady); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root, registryPath string, requireReady bool) error {
	payload, err := os.ReadFile(filepath.Join(root, registryPath))
	if err != nil {
		return fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return fmt.Errorf("decode profile registry: %w", err)
	}
	var required []string
	for _, profile := range registry.Profiles {
		required = append(required, profile.Cells...)
	}
	plan, err := finalv5profile.BuildCampaignPlan(registry, required, nil)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if requireReady && plan.ReadyDeployments() != len(plan.Deployments) {
		return errors.New("the campaign plan contains deployments that are not activatable yet")
	}
	return nil
}
