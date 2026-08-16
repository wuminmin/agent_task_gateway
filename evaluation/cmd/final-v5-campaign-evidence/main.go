// Command final-v5-campaign-evidence validates and merges one pilot
// per-profile campaign's deployment evidence into a credential-free manifest.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-campaign-evidence:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("final-v5-campaign-evidence", flag.ContinueOnError)
	root := flags.String("root", "", "campaign evidence root")
	planPath := flags.String("plan", "", "campaign plan JSON")
	campaignID := flags.String("campaign-id", "", "fixed campaign identity")
	commit := flags.String("submission-commit", "", "fixed submission commit")
	repetitions := flags.Int("repetitions", 1, "independent deployments per selected profile")
	profiles := flags.String("profiles", "", "optional comma-separated profile aliases")
	output := flags.String("out", "", "create-exclusive campaign manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *root == "" || *planPath == "" || *campaignID == "" || *commit == "" || *output == "" {
		return errors.New("root, plan, campaign-id, submission-commit and out are required")
	}
	payload, err := os.ReadFile(*planPath)
	if err != nil {
		return err
	}
	var plan finalv5profile.CampaignPlan
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return fmt.Errorf("decode campaign plan: %w", err)
	}
	digest := sha256.Sum256(payload)
	aliases, err := parseAliases(*profiles)
	if err != nil {
		return err
	}
	var records []string
	err = filepath.WalkDir(*root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("campaign evidence contains symlink %s", path)
		}
		if !entry.IsDir() && entry.Name() == "deployment-record.json" {
			records = append(records, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(records)
	manifest, err := finalv5profile.MergeProfileCampaignEvidence(plan, hex.EncodeToString(digest[:]),
		*root, *campaignID, *commit, *repetitions, aliases, records)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(*output)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(*output)
		return err
	}
	fmt.Printf("P30-STAGE: evidence_merge=pass deployments=%d profiles=%d repetitions=%d complete_matrix=%t\n",
		len(manifest.Deployments), len(manifest.ProfileAliases), manifest.Repetitions, manifest.CompleteMatrix)
	return nil
}

func parseAliases(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	aliases := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		alias := strings.TrimSpace(part)
		if alias == "" || seen[alias] {
			return nil, errors.New("profiles contains an empty or duplicate alias")
		}
		seen[alias] = true
		aliases = append(aliases, alias)
	}
	return aliases, nil
}
