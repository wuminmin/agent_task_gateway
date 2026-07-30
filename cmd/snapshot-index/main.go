package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

func main() {
	inputPath := flag.String("input", "", "path to taskgate-snapshot-index-input-v1 JSON")
	outputDirectory := flag.String("output-dir", "", "base directory for immutable publication bundles")
	allowExistingIdentical := flag.Bool("allow-existing-identical", false,
		"succeed only when an existing immutable publication is byte-identical")
	flag.Parse()
	if *inputPath == "" || *outputDirectory == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	inputFile, err := os.Open(*inputPath)
	if err != nil {
		fatal("open compiler input", err)
	}
	input, err := snapshotbundle.DecodeCompilerInput(inputFile)
	closeErr := inputFile.Close()
	if err != nil {
		fatal("read compiler input", err)
	}
	if closeErr != nil {
		fatal("close compiler input", closeErr)
	}
	if input.SourceRelation == "" {
		fatal("scan source relation", fmt.Errorf("source_relation is required by the publication CLI"))
	}
	dsn := strings.TrimSpace(os.Getenv("SNAPSHOT_POSTGRES_DSN"))
	if dsn == "" {
		fatal("scan source relation", fmt.Errorf("SNAPSHOT_POSTGRES_DSN is required for %s", input.SourceRelation))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	input, err = snapshotbundle.ScanPostgresSnapshot(ctx, input, dsn)
	stop()
	if err != nil {
		fatal("scan source relation", err)
	}
	limits := snapshotbundle.DefaultPublicationLimits()
	limits.AllowExistingIdentical = *allowExistingIdentical
	written, err := snapshotbundle.CompileOwnedToDirectory(&input, *outputDirectory, limits)
	if err != nil {
		fatal("compile and publish snapshot index", err)
	}
	manifest := written.Manifest.DictionaryManifest
	fmt.Printf("publication_dir=%s\nmanifest_digest=%s\ndictionary_digest=%s\nsidecar_digest=%s\ncold_payload_digest=%s\nhot_index_digest=%s\n",
		written.Directory, written.Manifest.ManifestDigest, manifest.DictionaryDigest, manifest.SidecarDigest,
		manifest.ColdPayloadDigest, manifest.HotIndexDigest)
}

func fatal(operation string, err error) {
	fmt.Fprintf(os.Stderr, "snapshot-index: %s: %v\n", operation, err)
	os.Exit(1)
}
