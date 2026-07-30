package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type options struct {
	configPath   string
	outputPath   string
	validateOnly bool
	prepare      bool
}

func main() {
	var opts options
	var printSourceDigest bool
	flag.StringVar(&opts.configPath, "config", "", "V4 root-family concurrency acceptance JSON")
	flag.StringVar(&opts.outputPath, "output", "", "new machine-readable report path")
	flag.BoolVar(&opts.validateOnly, "validate-only", false, "validate configuration without contacting services")
	flag.BoolVar(&opts.prepare, "prepare", false, "provision fresh root families through MCP/OA and write a prepared config")
	flag.BoolVar(&printSourceDigest, "print-source-digest", false, "print the bound implementation source digest")
	flag.Parse()
	if printSourceDigest {
		digest := sourceDigest()
		if !validDigest(digest) {
			fmt.Fprintln(os.Stderr, "cannot determine the V4 concurrency source digest")
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
	if opts.configPath == "" {
		return errors.New("-config is required")
	}
	raw, err := os.ReadFile(opts.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg concurrencyConfig
	if err := decodeStrictJSON(raw, &cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if opts.prepare && opts.validateOnly {
		return errors.New("-prepare cannot be combined with -validate-only")
	}
	requireTaskIDs := !opts.prepare
	if opts.validateOnly && !configContainsTaskIDs(cfg) {
		requireTaskIDs = false
	}
	if err := validateConfig(cfg, requireTaskIDs); err != nil {
		return err
	}
	if opts.prepare {
		if opts.outputPath == "" {
			return errors.New("-output is required for -prepare")
		}
		if _, err := os.Stat(opts.outputPath); err == nil {
			return fmt.Errorf("refusing to overwrite existing prepared config %s", opts.outputPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		prepared, err := provisionTaskFamilies(context.Background(), cfg)
		if err != nil {
			return err
		}
		if err := validateConfig(prepared, true); err != nil {
			return fmt.Errorf("prepared config is invalid: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(opts.outputPath), 0o755); err != nil {
			return err
		}
		if err := writeJSONExclusive(opts.outputPath, prepared); err != nil {
			return err
		}
		fmt.Printf("prepared %d V4 root families through public MCP/OA flow: %s\n", len(prepared.Cases), opts.outputPath)
		return nil
	}
	if opts.validateOnly {
		fmt.Printf("valid V4 concurrency config: %d cases\n", len(cfg.Cases))
		return nil
	}
	if opts.outputPath == "" {
		return errors.New("-output is required unless -validate-only is used")
	}
	if _, err := os.Stat(opts.outputPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing report %s", opts.outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.outputPath), 0o755); err != nil {
		return err
	}
	report, runErr := runConcurrencyCampaign(context.Background(), cfg, raw)
	if err := writeJSONExclusive(opts.outputPath, report); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("concurrency campaign failed; evidence retained at %s: %w", opts.outputPath, runErr)
	}
	if report.Acceptance != "pass" {
		return fmt.Errorf("concurrency acceptance failed; see %s", opts.outputPath)
	}
	fmt.Printf("V4 concurrency evidence written to %s (acceptance=pass)\n", opts.outputPath)
	return nil
}

func validateConfig(cfg concurrencyConfig, requireTaskIDs bool) error {
	if cfg.SchemaVersion != concurrencyConfigSchema {
		return fmt.Errorf("unsupported schema_version %d", cfg.SchemaVersion)
	}
	if strings.TrimSpace(cfg.Gateway.URL) == "" || strings.TrimSpace(cfg.Gateway.TokenEnv) == "" ||
		strings.TrimSpace(cfg.ControlDSNEnv) == "" {
		return errors.New("gateway URL/token env and Control PostgreSQL DSN env are required")
	}
	if len(cfg.Gateway.ContenderURLs) != 2 {
		return fmt.Errorf("exactly two contender Gateway URLs are required, got %d", len(cfg.Gateway.ContenderURLs))
	}
	seenGatewayURLs := make(map[string]struct{}, len(cfg.Gateway.ContenderURLs))
	for _, gatewayURL := range cfg.Gateway.ContenderURLs {
		gatewayURL = strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
		if gatewayURL == "" {
			return errors.New("contender Gateway URL cannot be empty")
		}
		if _, duplicate := seenGatewayURLs[gatewayURL]; duplicate {
			return fmt.Errorf("duplicate contender Gateway URL %q", gatewayURL)
		}
		seenGatewayURLs[gatewayURL] = struct{}{}
	}
	if cfg.RequestTimeoutMS <= 0 || cfg.LockWaitTimeoutMS <= 0 || cfg.LockWaitTimeoutMS >= cfg.RequestTimeoutMS {
		return errors.New("timeouts must be positive and lock_wait_timeout_ms must be less than request_timeout_ms")
	}
	if len(cfg.Cases) == 0 {
		return errors.New("at least one concurrency case is required")
	}
	requiredLevels := map[int]bool{1: false, 4: false, 8: false, 16: false}
	requiredDimensions := map[string]bool{"release": false, "influence": false, "outcome": false}
	caseIDs := make(map[string]struct{})
	rootIDs := make(map[string]struct{})
	operationTasks := make(map[string]string)
	if cfg.Provision != nil {
		if strings.TrimSpace(cfg.Provision.OAURL) == "" || strings.TrimSpace(cfg.Provision.AlicePasswordEnv) == "" ||
			strings.TrimSpace(cfg.Provision.BobPasswordEnv) == "" || len(cfg.Provision.DataProducts) == 0 ||
			len(cfg.Provision.Columns) == 0 || cfg.Provision.Scopes == nil {
			return errors.New("provision requires OA URL/password envs plus explicit products, columns, and scopes")
		}
	}
	if !requireTaskIDs && cfg.Provision == nil {
		return errors.New("-prepare requires a provision contract")
	}
	for index, one := range cfg.Cases {
		if strings.TrimSpace(one.ID) == "" {
			return fmt.Errorf("case %d has no id", index)
		}
		if _, duplicate := caseIDs[one.ID]; duplicate {
			return fmt.Errorf("duplicate case id %q", one.ID)
		}
		caseIDs[one.ID] = struct{}{}
		if _, supported := requiredLevels[one.Concurrency]; !supported {
			return fmt.Errorf("case %s has unsupported concurrency %d", one.ID, one.Concurrency)
		}
		requiredLevels[one.Concurrency] = true
		if _, supported := requiredDimensions[one.BoundaryDimension]; !supported {
			return fmt.Errorf("case %s has unsupported boundary_dimension %q", one.ID, one.BoundaryDimension)
		}
		requiredDimensions[one.BoundaryDimension] = true
		if requireTaskIDs {
			if strings.TrimSpace(one.RootTaskID) == "" {
				return fmt.Errorf("case %s has no root_task_id", one.ID)
			}
			if _, duplicate := rootIDs[one.RootTaskID]; duplicate {
				return fmt.Errorf("root_task_id %q is reused; each cell requires a fresh independent root", one.RootTaskID)
			}
			rootIDs[one.RootTaskID] = struct{}{}
			if len(one.ContenderTaskIDs) != one.Concurrency {
				return fmt.Errorf("case %s has %d contender tasks, want %d", one.ID, len(one.ContenderTaskIDs), one.Concurrency)
			}
			tasks := append([]string{one.PrefixTaskID, one.OverflowTaskID}, one.ContenderTaskIDs...)
			for _, taskID := range tasks {
				if strings.TrimSpace(taskID) == "" {
					return fmt.Errorf("case %s contains an empty operation task id", one.ID)
				}
				if prior, duplicate := operationTasks[taskID]; duplicate {
					return fmt.Errorf("operation task %q is reused by cases %s and %s", taskID, prior, one.ID)
				}
				operationTasks[taskID] = one.ID
			}
		} else if one.RootTaskID != "" || one.PrefixTaskID != "" || one.OverflowTaskID != "" || len(one.ContenderTaskIDs) != 0 {
			return fmt.Errorf("case %s prepare template must not contain task IDs", one.ID)
		}
		for name, plan := range map[string]json.RawMessage{"prefix": one.PrefixPlan,
			"contender": one.ContenderPlan, "overflow": one.OverflowPlan} {
			if !validJSONObject(plan) {
				return fmt.Errorf("case %s has invalid %s_plan", one.ID, name)
			}
		}
		if sha256Hex(one.PrefixPlan) == sha256Hex(one.ContenderPlan) ||
			sha256Hex(one.ContenderPlan) == sha256Hex(one.OverflowPlan) ||
			sha256Hex(one.PrefixPlan) == sha256Hex(one.OverflowPlan) {
			return fmt.Errorf("case %s plans must be byte-distinct", one.ID)
		}
		for name, value := range map[string]int64{
			"before.release": one.BeforeUsed.Release, "before.influence": one.BeforeUsed.Influence,
			"before.outcome": one.BeforeUsed.Outcome, "budget.release": one.AtBudget.Release,
			"budget.influence": one.AtBudget.Influence, "budget.outcome": one.AtBudget.Outcome,
		} {
			if value < 0 || (strings.HasPrefix(name, "budget.") && value == 0) {
				return fmt.Errorf("case %s has invalid %s=%d", one.ID, name, value)
			}
		}
		delta := one.AtBudget.subtract(one.BeforeUsed)
		if delta.Release <= 0 || delta.Influence <= 0 || delta.Outcome <= 0 {
			return fmt.Errorf("case %s contender must advance all three dimensions", one.ID)
		}
		if one.AtBudget.dimension(one.BoundaryDimension)-one.BeforeUsed.dimension(one.BoundaryDimension) != 1 {
			return fmt.Errorf("case %s does not encode an exact B-1 boundary in %s", one.ID, one.BoundaryDimension)
		}
	}
	var missing []string
	for level, present := range requiredLevels {
		if !present {
			missing = append(missing, fmt.Sprintf("concurrency=%d", level))
		}
	}
	for dimension, present := range requiredDimensions {
		if !present {
			missing = append(missing, "boundary="+dimension)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return errors.New("configuration omits required coverage: " + strings.Join(missing, ", "))
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(raw)
}

func configContainsTaskIDs(cfg concurrencyConfig) bool {
	for _, one := range cfg.Cases {
		if one.RootTaskID != "" || one.PrefixTaskID != "" || one.OverflowTaskID != "" || len(one.ContenderTaskIDs) > 0 {
			return true
		}
	}
	return false
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
