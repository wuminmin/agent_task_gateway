// Command final-v5-publication-binding generates and validates the private
// P4.0-E1 12/6/105 publication binding review candidate.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "expected generate or validate")
		return 2
	}
	var (
		summary finalv5publication.GenerationSummary
		err     error
	)
	switch arguments[0] {
	case "generate":
		flags := flag.NewFlagSet("final-v5-publication-binding generate", flag.ContinueOnError)
		flags.SetOutput(stderr)
		root := flags.String("repo-root", ".", "TaskGate repository root")
		output := flags.String("output-dir", "", "new private publication review directory")
		artifacts := flags.String("artifact-dir", "", "existing private mode-0700 oracle temporary directory")
		if err := flags.Parse(arguments[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || strings.TrimSpace(*output) == "" || strings.TrimSpace(*artifacts) == "" {
			fmt.Fprintln(stderr, "repo-root, output-dir, and artifact-dir are the only generate inputs; output-dir and artifact-dir are required")
			return 2
		}
		dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
		if dsn == "" {
			dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
		}
		if dsn == "" {
			fmt.Fprintln(stderr, "BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN is required")
			return 1
		}
		summary, err = finalv5publication.GeneratePublicationBinding(context.Background(),
			finalv5publication.GenerateOptions{RepositoryRoot: *root, OutputDirectory: *output,
				ArtifactRoot: *artifacts, BusinessDSN: dsn})
	case "generate-current-binding":
		flags := flag.NewFlagSet("final-v5-publication-binding generate-current-binding", flag.ContinueOnError)
		flags.SetOutput(stderr)
		root := flags.String("repo-root", ".", "TaskGate repository root")
		output := flags.String("output-dir", "", "new private current-Catalog Dataset Binding directory")
		artifacts := flags.String("artifact-dir", "", "existing private mode-0700 oracle temporary directory")
		if err := flags.Parse(arguments[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || strings.TrimSpace(*output) == "" || strings.TrimSpace(*artifacts) == "" {
			fmt.Fprintln(stderr, "repo-root, output-dir, and artifact-dir are the only generate-current-binding inputs; output-dir and artifact-dir are required")
			return 2
		}
		dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
		if dsn == "" {
			dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
		}
		if dsn == "" {
			fmt.Fprintln(stderr, "BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN is required")
			return 1
		}
		current, currentErr := finalv5publication.GenerateCurrentDatasetBinding(context.Background(),
			finalv5publication.CurrentDatasetBindingOptions{RepositoryRoot: *root,
				OutputDirectory: *output, ArtifactRoot: *artifacts, BusinessDSN: dsn})
		if currentErr != nil {
			fmt.Fprintln(stderr, currentErr)
			return 1
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(current); err != nil {
			fmt.Fprintln(stderr, errors.New("encode current Dataset Binding summary"))
			return 1
		}
		return 0
	case "validate":
		flags := flag.NewFlagSet("final-v5-publication-binding validate", flag.ContinueOnError)
		flags.SetOutput(stderr)
		root := flags.String("repo-root", ".", "TaskGate repository root")
		input := flags.String("input-dir", "", "private publication review directory")
		if err := flags.Parse(arguments[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || strings.TrimSpace(*input) == "" {
			fmt.Fprintln(stderr, "repo-root and input-dir are the only validate inputs; input-dir is required")
			return 2
		}
		summary, err = finalv5publication.ValidatePublicationOutput(*root, *input, nil)
	default:
		fmt.Fprintln(stderr, "expected generate, generate-current-binding, or validate")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		fmt.Fprintln(stderr, errors.New("encode publication binding summary"))
		return 1
	}
	return 0
}
