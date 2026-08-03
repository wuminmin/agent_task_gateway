package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

type inputPaths []string

func (paths *inputPaths) String() string { return strings.Join(*paths, ",") }

func (paths *inputPaths) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("input path is empty")
	}
	*paths = append(*paths, value)
	return nil
}

func main() {
	var inputPath inputPaths
	flag.Var(&inputPath, "input", "path to taskgate-snapshot-index-input-v1 JSON (repeatable)")
	outputDirectory := flag.String("output-dir", "", "base directory for immutable publication bundles")
	allowExistingIdentical := flag.Bool("allow-existing-identical", false,
		"succeed only when an existing immutable publication is byte-identical")
	flag.Parse()
	if len(inputPath) == 0 || *outputDirectory == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	dsn := strings.TrimSpace(os.Getenv("SNAPSHOT_POSTGRES_DSN"))
	if dsn == "" {
		fatal("scan source relation", fmt.Errorf("SNAPSHOT_POSTGRES_DSN is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	limits := snapshotbundle.DefaultPublicationLimits()
	limits.AllowExistingIdentical = *allowExistingIdentical
	for _, path := range inputPath {
		if err := compileOne(ctx, path, dsn, *outputDirectory, limits); err != nil {
			fatal("compile and publish snapshot index", fmt.Errorf("%s: %w", path, err))
		}
	}
}

func compileOne(ctx context.Context, inputPath, dsn, outputDirectory string, limits snapshotbundle.PublicationLimits) error {
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open compiler input: %w", err)
	}
	input, decodeErr := snapshotbundle.DecodeCompilerInput(inputFile)
	closeErr := inputFile.Close()
	if decodeErr != nil || closeErr != nil {
		return fmt.Errorf("read compiler input: %w", errors.Join(decodeErr, closeErr))
	}
	if input.SourceRelation == "" {
		return fmt.Errorf("source_relation is required by the publication CLI")
	}
	input, err = snapshotbundle.ScanPostgresSnapshot(ctx, input, dsn)
	if err != nil {
		return fmt.Errorf("scan source relation: %w", err)
	}
	written, err := snapshotbundle.CompileOwnedToDirectory(&input, outputDirectory, limits)
	if err != nil {
		return err
	}
	manifest := written.Manifest.DictionaryManifest
	fmt.Printf("publication_dir=%s\nmanifest_digest=%s\ndictionary_digest=%s\nsidecar_digest=%s\ncold_payload_digest=%s\nhot_index_digest=%s\n",
		written.Directory, written.Manifest.ManifestDigest, manifest.DictionaryDigest, manifest.SidecarDigest,
		manifest.ColdPayloadDigest, manifest.HotIndexDigest)
	return nil
}

func fatal(operation string, err error) {
	fmt.Fprintf(os.Stderr, "snapshot-index: %s: %v\n", operation, err)
	os.Exit(1)
}
