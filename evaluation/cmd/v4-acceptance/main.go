package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type options struct {
	configPath            string
	outputPath            string
	prepareNarrowTaskPool string
	prepareFullTaskPool   string
	fullEnvironmentPath   string
	fullEnvironmentSHA256 string
	fullBaselinePath      string
	fullBaselineSHA256    string
	fullCandidatePath     string
	fullCandidateSHA256   string
	requireComplete       bool
	validateOnly          bool
}

func main() {
	var opts options
	var printSourceDigest bool
	flag.StringVar(&opts.configPath, "config", "", "V4 acceptance JSON configuration")
	flag.StringVar(&opts.outputPath, "output", "", "new machine-readable result path")
	flag.StringVar(&opts.prepareNarrowTaskPool, "prepare-narrow-task-pool", "",
		"inject a 20-root maximum-point task pool into the narrow acceptance template")
	flag.StringVar(&opts.prepareFullTaskPool, "prepare-full-task-pool", "",
		"inject a 140-root task pool into the seven-case full-matrix acceptance template")
	flag.StringVar(&opts.fullEnvironmentPath, "full-environment-path", "",
		"fixed-environment JSON evidence used by full-matrix preparation")
	flag.StringVar(&opts.fullEnvironmentSHA256, "full-environment-sha256", "",
		"expected lowercase SHA-256 of full-environment-path")
	flag.StringVar(&opts.fullBaselinePath, "full-baseline-path", "",
		"V2 small-query benchmark results JSON used by full-matrix preparation")
	flag.StringVar(&opts.fullBaselineSHA256, "full-baseline-sha256", "",
		"expected lowercase SHA-256 of full-baseline-path")
	flag.StringVar(&opts.fullCandidatePath, "full-candidate-path", "",
		"V4 small-query benchmark results JSON used by full-matrix preparation")
	flag.StringVar(&opts.fullCandidateSHA256, "full-candidate-sha256", "",
		"expected lowercase SHA-256 of full-candidate-path")
	flag.BoolVar(&opts.requireComplete, "require-complete", false, "fail when any gate is unmeasured")
	flag.BoolVar(&opts.validateOnly, "validate-only", false, "validate configuration without contacting services")
	flag.BoolVar(&printSourceDigest, "print-source-digest", false,
		"print the deterministic source digest used for acceptance evidence")
	flag.Parse()
	if printSourceDigest {
		digest := sourceDigest()
		if !validSourceDigest(digest) {
			fmt.Fprintln(os.Stderr, "cannot determine a complete V4 acceptance source digest")
			os.Exit(1)
		}
		fmt.Println(digest)
		return
	}
	if err := execute(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(opts options) error {
	fullEvidenceConfigured := opts.fullEnvironmentPath != "" || opts.fullEnvironmentSHA256 != "" ||
		opts.fullBaselinePath != "" || opts.fullBaselineSHA256 != "" ||
		opts.fullCandidatePath != "" || opts.fullCandidateSHA256 != ""
	if opts.prepareFullTaskPool == "" && fullEvidenceConfigured {
		return errors.New("full evidence flags require -prepare-full-task-pool")
	}
	if opts.configPath == "" {
		return errors.New("-config is required")
	}
	raw, err := os.ReadFile(opts.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg config
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if opts.prepareNarrowTaskPool != "" && opts.prepareFullTaskPool != "" {
		return errors.New("only one task-pool preparation mode may be selected")
	}
	if opts.prepareNarrowTaskPool != "" || opts.prepareFullTaskPool != "" {
		if opts.validateOnly || opts.requireComplete {
			return errors.New("config preparation cannot be combined with -validate-only or -require-complete")
		}
		if opts.outputPath == "" {
			return errors.New("-output is required when preparing a config")
		}
		prepared := config{}
		if opts.prepareNarrowTaskPool != "" {
			prepared, err = prepareNarrowConfig(cfg, opts.prepareNarrowTaskPool)
		} else {
			prepared, err = prepareFullConfig(cfg, opts.prepareFullTaskPool, fullPreparationEvidence{
				Environment: fullBoundArtifact{Path: opts.fullEnvironmentPath, SHA256: opts.fullEnvironmentSHA256},
				Baseline:    fullBoundArtifact{Path: opts.fullBaselinePath, SHA256: opts.fullBaselineSHA256},
				Candidate:   fullBoundArtifact{Path: opts.fullCandidatePath, SHA256: opts.fullCandidateSHA256},
			})
		}
		if err != nil {
			return err
		}
		if err := validateConfig(prepared); err != nil {
			return fmt.Errorf("prepared config: %w", err)
		}
		if err := writeJSONExclusive(opts.outputPath, prepared); err != nil {
			return err
		}
		mode := "narrow"
		if opts.prepareFullTaskPool != "" {
			mode = "full-matrix"
		}
		fmt.Printf("prepared V4 %s acceptance config with %d fresh roots: %s\n",
			mode, trialCount(prepared.Cases), opts.outputPath)
		return nil
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if opts.validateOnly {
		fmt.Printf("valid V4 acceptance config: %d cases, %d trials\n", len(cfg.Cases), trialCount(cfg.Cases))
		return nil
	}
	if opts.outputPath == "" {
		return errors.New("-output is required unless -validate-only is used")
	}
	if _, err := os.Stat(opts.outputPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing result %s", opts.outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.outputPath), 0o755); err != nil {
		return err
	}
	ctx := context.Background()
	result, runErr := runCampaign(ctx, cfg, raw, filepath.Dir(opts.outputPath))
	result.FinishedAt = time.Now().UTC()
	if err := writeJSONExclusive(opts.outputPath, result); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("campaign failed; evidence retained at %s: %w", opts.outputPath, runErr)
	}
	if result.Acceptance == "fail" {
		return fmt.Errorf("one or more acceptance gates failed; see %s", opts.outputPath)
	}
	if opts.requireComplete && result.Acceptance != "pass" {
		return fmt.Errorf("acceptance is %s because at least one gate is unmeasured; see %s", result.Acceptance, opts.outputPath)
	}
	fmt.Printf("V4 acceptance evidence written to %s (acceptance=%s)\n", opts.outputPath, result.Acceptance)
	return nil
}

func validateConfig(cfg config) error {
	if cfg.SchemaVersion != configSchema {
		return fmt.Errorf("unsupported config schema %d", cfg.SchemaVersion)
	}
	if strings.TrimSpace(cfg.Gateway.URL) == "" || strings.TrimSpace(cfg.Gateway.TokenEnv) == "" ||
		strings.TrimSpace(cfg.BusinessDSNEnv) == "" || strings.TrimSpace(cfg.ControlDSNEnv) == "" {
		return errors.New("gateway url/token env and both PostgreSQL DSN env names are required")
	}
	if cfg.RequestTimeoutMS <= 0 || cfg.StatementTimeoutMS <= 0 {
		return errors.New("positive request_timeout_ms and statement_timeout_ms are required")
	}
	if cfg.OverlapTolerancePoint <= 0 || cfg.OverlapTolerancePoint > 5 {
		return errors.New("overlap tolerance must be in (0,5] percentage points")
	}
	if len(cfg.Cases) == 0 {
		return errors.New("at least one workload case is required")
	}
	seen := make(map[string]struct{}, len(cfg.Cases))
	usedTasks := make(map[string]string)
	for index, one := range cfg.Cases {
		if strings.TrimSpace(one.ID) == "" {
			return fmt.Errorf("case %d has no id", index)
		}
		if _, ok := seen[one.ID]; ok {
			return fmt.Errorf("duplicate case id %q", one.ID)
		}
		seen[one.ID] = struct{}{}
		switch one.Shape {
		case "scan", "join_group", "union", "page":
		default:
			return fmt.Errorf("case %s has unsupported shape %q", one.ID, one.Shape)
		}
		if one.TargetOverlapPercent < 0 || one.TargetOverlapPercent > 100 {
			return fmt.Errorf("case %s overlap target is outside [0,100]", one.ID)
		}
		dimension := one.OverlapDimension
		if dimension == "" {
			dimension = "influence"
		}
		switch dimension {
		case "release", "influence", "outcome", "all":
		default:
			return fmt.Errorf("case %s has invalid overlap_dimension %q", one.ID, dimension)
		}
		if len(one.TaskIDs) == 0 || !validJSONObject(one.Plan) || strings.TrimSpace(one.DirectSQL) == "" {
			return fmt.Errorf("case %s requires task_ids, a valid plan, and direct_sql", one.ID)
		}
		for _, plan := range one.SetupPlans {
			if !validJSONObject(plan) {
				return fmt.Errorf("case %s contains an invalid setup plan", one.ID)
			}
		}
		for _, taskID := range one.TaskIDs {
			if strings.TrimSpace(taskID) == "" {
				return fmt.Errorf("case %s contains an empty task id", one.ID)
			}
			if prior, duplicate := usedTasks[taskID]; duplicate {
				return fmt.Errorf("task id is reused by cases %s and %s; fresh-root trials must be independent", prior, one.ID)
			}
			usedTasks[taskID] = one.ID
		}
	}
	for name, metric := range map[string]*commandMetric{"index_build": cfg.IndexBuild,
		"activation_verification": cfg.ActivationVerification, "activation": cfg.Activation} {
		if metric == nil {
			continue
		}
		if len(metric.Argv) == 0 || metric.Runs < 0 || metric.TimeoutMS < 0 {
			return fmt.Errorf("%s has an invalid command contract", name)
		}
	}
	if cfg.Activation != nil && cfg.Activation.WarmVerified {
		if cfg.ActivationVerification == nil ||
			!commandContainsToken(cfg.ActivationVerification.Argv, "{{verification_receipt}}") ||
			!commandContainsToken(cfg.Activation.Argv, "{{verification_receipt}}") ||
			!commandContainsToken(cfg.Activation.Argv, "{{verification_receipt_sha256}}") {
			return errors.New("warm verified activation requires a strict verification command and bound receipt placeholders")
		}
	}
	if cfg.Observer != nil && (len(cfg.Observer.Argv) == 0 || cfg.Observer.TimeoutMS < 0) {
		return errors.New("observer has an invalid command contract")
	}
	if cfg.EnvironmentManifest != nil && (strings.TrimSpace(cfg.EnvironmentManifest.Path) == "" ||
		(len(cfg.EnvironmentManifest.SHA256) != 64 || strings.ToLower(cfg.EnvironmentManifest.SHA256) != cfg.EnvironmentManifest.SHA256)) {
		return errors.New("environment_manifest requires a path and lowercase SHA-256")
	}
	if cfg.EnvironmentManifest != nil {
		if _, err := hex.DecodeString(cfg.EnvironmentManifest.SHA256); err != nil {
			return errors.New("environment_manifest SHA-256 is not hexadecimal")
		}
	}
	return nil
}

func commandContainsToken(argv []string, token string) bool {
	for _, value := range argv {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

func validJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(raw)
}

func trialCount(cases []workloadCase) int {
	total := 0
	for _, one := range cases {
		total += len(one.TaskIDs)
	}
	return total
}

func writeJSONExclusive(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(value)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func hashTask(taskID string) string {
	return sha256Hex([]byte(taskID))
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
