package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-split-publication:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("final-v5-split-publication", flag.ContinueOnError)
	planPath := flags.String("plan", "", "formal split campaign plan")
	root := flags.String("root", "", "completed campaign root")
	output := flags.String("out", "", "create-exclusive final campaign manifest")
	validatePlan := flags.Bool("validate-plan", false, "zero-deployment static validation")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *planPath == "" {
		return errors.New("-plan is required and positional arguments are forbidden")
	}
	if *validatePlan {
		if *root != "" || *output != "" {
			return errors.New("-validate-plan cannot be combined with -root or -out")
		}
		payload, err := os.ReadFile(*planPath)
		if err != nil {
			return err
		}
		var plan finalv5profile.CampaignPlan
		if err := experiment.StrictJSON(payload, &plan); err != nil {
			return err
		}
		if err := experiment.ValidateSplitPublicationPlan(plan); err != nil {
			return err
		}
		fmt.Println("P62B-STAGE: publication_dry=pass profile_cells=129 scale_nonprofile=38 compiler_nonprofile=11 total=178 fresh_executions=3 deployments=0")
		return nil
	}
	if *root == "" || *output == "" {
		return errors.New("finalization requires -root and -out")
	}
	summary, err := experiment.FinalizeSplitPublicationCampaign(*root, *planPath)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
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
	fmt.Printf("P62B-STAGE: publication_finalize=pass profile_cells=%d scale_nonprofile=%d compiler_nonprofile=%d total=%d\n",
		summary.ProfileCells, summary.ScaleNonProfile, summary.CompilerNonProfile, summary.TotalCells)
	return nil
}
