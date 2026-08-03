// Command final-v5-contract-sql-check proves every SQL and plan artifact the
// Final-V5 Contract Index names can actually parse, execute or compile.
//
// A contract used to be verified by digest and structure alone, so three
// releases shipped a dataset probe that PostgreSQL could not parse. This gate
// closes that gap: it reads the Contract Index rather than a hand-written file
// list, provisions a throwaway PostgreSQL 16 database with the contract's own
// dataset generator, runs the contract's own probe bytes, renders and plans
// every Direct template, and lowers and compiles every BDG template through the
// production SQL path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5sqlcheck"
)

const manifestPath = "evaluation/final-v5-wsl2/contracts/sql-executability-v1.json"

func main() {
	var root, output string
	var requireLive bool
	flag.StringVar(&root, "root", ".", "repository root")
	flag.StringVar(&output, "manifest-out", manifestPath, "executability manifest path, relative to root")
	flag.BoolVar(&requireLive, "require-live", false,
		"fail instead of skipping when no disposable PostgreSQL is configured")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "final-v5-contract-sql-check: positional arguments are not accepted")
		os.Exit(2)
	}
	if err := run(root, output, requireLive); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-contract-sql-check:", err)
		os.Exit(1)
	}
}

func run(root, output string, requireLive bool) error {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return fmt.Errorf("contract bridge: %w", err)
	}
	adminDSN, dsnErr := finalv5sqlcheck.AdminDSN()
	if dsnErr != nil {
		if requireLive {
			return dsnErr
		}
		// Without a disposable deployment the gate cannot make its claim, so it
		// says so and makes none. It never reports a pass it did not establish.
		fmt.Printf("contract SQL executability: SKIPPED (%s is not set)\n", finalv5sqlcheck.AdminDSNEnv)
		return nil
	}
	manifest, err := finalv5sqlcheck.Run(context.Background(), runtime, adminDSN)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(root, output)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	for _, check := range manifest.Checked {
		if check.Status == "fail" {
			fmt.Fprintf(os.Stderr, "  FAIL %-46s %s\n", check.Path, check.Detail)
		}
	}
	fmt.Printf("contract SQL executability: %s (%s, PostgreSQL %s, %d artifacts, %d rendered cells, %d failed)\n",
		manifest.Status, manifest.ContractRelease, manifest.PostgreSQLVersion,
		manifest.CheckedCount, manifest.RenderedCellCount, manifest.FailedCount)
	if manifest.Status != "pass" {
		return errors.New("contract-indexed SQL is not executable")
	}
	return nil
}
