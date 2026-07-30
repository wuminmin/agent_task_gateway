// v4-distribution runs or independently validates the deterministic TaskGate
// V4 ordinal bitmap distribution kernel. Its output is supplemental kernel
// evidence and must not be presented as Gateway/SQL end-to-end latency.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/v4distribution"
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(arguments []string) error {
	defaults := v4distribution.DefaultConfig()
	flags := flag.NewFlagSet("v4-distribution", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	output := flags.String("output", "", "new JSON kernel report path")
	validate := flags.String("validate", "", "independently validate an existing JSON report")
	cardinality := flags.Uint64("cardinality", defaults.Cardinality, "exact effect cardinality (multiple of 100)")
	runs := flags.Int("runs", defaults.Runs, "raw timing samples per matrix cell")
	clusters := flags.Uint("cluster-count", uint(defaults.ClusterCount), "deterministic clustered-distribution cluster count")
	seed := flags.Uint("random-seed", uint(defaults.RandomSeed), "deterministic random-sparse permutation offset")
	replayLookups := flags.Int("replay-lookups-per-run", defaults.ReplayLookupsPerRun, "digest lookups in each timed replay batch")
	maxHeap := flags.Uint64("max-peak-heap-bytes", defaults.MaxPeakHeapBytes, "fail-closed kernel HeapAlloc ceiling")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *validate != "" {
		if *output != "" {
			return errors.New("-validate and -output are mutually exclusive")
		}
		report, err := v4distribution.ReadAndValidate(*validate)
		if err != nil {
			return err
		}
		fmt.Printf("valid V4 bitmap distribution kernel: cells=%d cardinality=%d acceptance_eligible=%t matrix_sha256=%s\n",
			len(report.Cells), report.Configuration.Cardinality, report.AcceptanceEligible, report.MatrixSHA256)
		return nil
	}
	if *output == "" {
		return errors.New("-output is required unless -validate is used")
	}
	if *clusters > uint(^uint32(0)) || *seed > uint(^uint32(0)) {
		return errors.New("cluster-count and random-seed must fit uint32")
	}
	config := v4distribution.Config{Cardinality: *cardinality, Runs: *runs, ClusterCount: uint32(*clusters),
		RandomSeed: uint32(*seed), ReplayLookupsPerRun: *replayLookups, MaxPeakHeapBytes: *maxHeap}
	report, err := v4distribution.Run(config)
	if err != nil {
		return err
	}
	if err := writeJSONExclusive(*output, report); err != nil {
		return err
	}
	fmt.Printf("V4 bitmap distribution kernel written to %s (cells=%d acceptance_eligible=%t matrix_sha256=%s)\n",
		*output, len(report.Cells), report.AcceptanceEligible, report.MatrixSHA256)
	return nil
}

func writeJSONExclusive(path string, value any) error {
	if path == "" || filepath.Clean(path) == "." {
		return errors.New("output path is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create output without overwrite: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}
