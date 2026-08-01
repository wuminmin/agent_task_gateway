package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: final-v5 <validate|finalize|evidence|smoke|record-environment|record-deployment>")
		return 2
	}
	switch os.Args[1] {
	case "validate":
		return validate(os.Args[2:])
	case "finalize":
		return finalize(os.Args[2:])
	case "evidence":
		return evidence(os.Args[2:])
	case "smoke":
		return smoke(os.Args[2:])
	case "record-environment":
		return recordEnvironment(os.Args[2:])
	case "record-deployment":
		return recordDeployment(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		return 2
	}
}

func recordDeployment(args []string) int {
	f := flag.NewFlagSet("record-deployment", flag.ContinueOnError)
	output := f.String("output", "", "")
	campaign := f.String("campaign-id", "", "")
	deployment := f.String("deployment-id", "", "")
	environmentSHA := f.String("environment-sha256", "", "")
	windowsEnvironmentSHA := f.String("windows-environment-sha256", "", "")
	vmstatBeforeSHA := f.String("vmstat-before-sha256", "", "")
	vmstatAfterSHA := f.String("vmstat-after-sha256", "", "")
	started := f.String("started-at", "", "")
	finished := f.String("finished-at", "", "")
	exitStatus := f.Int("exit-status", -1, "")
	swapIn := f.Int64("swap-in-delta", -1, "")
	swapOut := f.Int64("swap-out-delta", -1, "")
	restarts := f.Int64("unexpected-container-restarts", -1, "")
	oom := f.Bool("oom", false, "")
	if f.Parse(args) != nil || *output == "" || *campaign == "" || *deployment == "" || *exitStatus < 0 {
		return 2
	}
	manifest := experiment.DeploymentManifest{SchemaVersion: 1, CampaignID: *campaign, DeploymentID: *deployment, FreshDeployment: true, EnvironmentSHA256: *environmentSHA, WindowsEnvironmentSHA256: *windowsEnvironmentSHA, VMStatBeforeSHA256: *vmstatBeforeSHA, VMStatAfterSHA256: *vmstatAfterSHA, StartedAt: *started, FinishedAt: *finished, ExitStatus: *exitStatus, SwapInDelta: *swapIn, SwapOutDelta: *swapOut, OOM: *oom, UnexpectedContainerRestarts: *restarts}
	if err := experiment.WriteDeployment(*output, manifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func validate(args []string) int {
	f := flag.NewFlagSet("validate", flag.ContinueOnError)
	root := f.String("root", "evaluation/final-v5-wsl2", "")
	if f.Parse(args) != nil {
		return 2
	}
	failed := false
	for _, pattern := range []string{"config/*.json", "schema/*.json"} {
		paths, _ := filepath.Glob(filepath.Join(*root, pattern))
		if len(paths) == 0 {
			fmt.Fprintln(os.Stderr, "no files for", pattern)
			failed = true
		}
		for _, path := range paths {
			value, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				failed = true
				continue
			}
			var decoded any
			if err := json.Unmarshal(value, &decoded); err != nil {
				fmt.Fprintln(os.Stderr, path, err)
				failed = true
			}
		}
	}
	configs, _ := filepath.Glob(filepath.Join(*root, "config", "*.json"))
	for _, path := range configs {
		var generic struct {
			ExperimentID string `json:"experiment_id"`
		}
		value, _ := os.ReadFile(path)
		_ = json.Unmarshal(value, &generic)
		if _, _, err := experiment.LoadConfig(path, generic.ExperimentID); err != nil {
			fmt.Fprintln(os.Stderr, path, err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Println("final V5 protocol/config/schema validation: pass")
	return 0
}

func finalize(args []string) int {
	f := flag.NewFlagSet("finalize", flag.ContinueOnError)
	runDir := f.String("run-dir", "", "")
	allowIncompletePilot := f.Bool("allow-incomplete-pilot", false, "accept an incomplete pilot process exit for smoke validation")
	if f.Parse(args) != nil || *runDir == "" {
		return 2
	}
	summary, err := experiment.FinalizeRun(*runDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(summary)
	if summary.Status != "pass" && !(*allowIncompletePilot && summary.CampaignClass == "pilot" && !summary.PublicationEligible) {
		return 1
	}
	return 0
}

func evidence(args []string) int {
	f := flag.NewFlagSet("evidence", flag.ContinueOnError)
	runDir := f.String("run-dir", "", "")
	if f.Parse(args) != nil || *runDir == "" {
		return 2
	}
	manifest, err := experiment.SealRun(*runDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(manifest)
	return 0
}
func smoke(args []string) int {
	f := flag.NewFlagSet("smoke", flag.ContinueOnError)
	configPath := f.String("config", "evaluation/final-v5-wsl2/config/pilot.example.json", "")
	runDir := f.String("run-dir", "", "")
	if f.Parse(args) != nil || *runDir == "" {
		return 2
	}
	config, _, err := experiment.LoadConfig(*configPath, "baseline")
	if err == nil {
		err = experiment.WriteSmoke(config, *runDir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("tiny smoke pass; publication_eligible=false")
	return 0
}
func recordEnvironment(args []string) int {
	f := flag.NewFlagSet("record-environment", flag.ContinueOnError)
	repo := f.String("repo", ".", "")
	campaign := f.String("campaign-id", "", "")
	deployment := f.String("deployment-id", "", "")
	output := f.String("output", "", "")
	bindings := f.String("dataset-bindings", "", "")
	eligible := f.Bool("publication-eligible", false, "")
	if f.Parse(args) != nil || *campaign == "" || *deployment == "" || *output == "" {
		return 2
	}
	datasets, err := experiment.ReadDatasetBindings(*bindings)
	if err == nil {
		var manifest experiment.EnvironmentManifest
		manifest, err = experiment.RecordEnvironment(*repo, *campaign, *deployment, *eligible, datasets)
		if err == nil {
			err = experiment.WriteEnvironment(*output, manifest)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
