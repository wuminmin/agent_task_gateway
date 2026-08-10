// Command final-v5-publication-review builds credential-free, pre-run review
// material for the frozen Final-V5 exposure-scale ordinal publication.
// Database access is fixed in regular source and credentials are accepted only
// through the repository's established environment variables.
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
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("final-v5-publication-review", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repositoryRoot := flags.String("repo-root", ".", "TaskGate repository root")
	outputDirectory := flags.String("output-dir", "", "new credential-free review-material directory")
	artifactDirectory := flags.String("artifact-dir", "", "private temporary root for generated HOT/COLD/sidecar artifacts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*outputDirectory) == "" || strings.TrimSpace(*artifactDirectory) == "" {
		fmt.Fprintln(stderr, "repo-root, output-dir, and artifact-dir are the only accepted inputs; output-dir and artifact-dir are required")
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
	report, err := generateReview(context.Background(), generateOptions{
		RepositoryRoot:  *repositoryRoot,
		OutputDirectory: *outputDirectory,
		ArtifactRoot:    *artifactDirectory,
		DSN:             dsn,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(stderr, errors.New("encode publication review report"))
		return 1
	}
	return 0
}
