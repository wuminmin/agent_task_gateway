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
	bundle, err := snapshotbundle.Compile(input)
	if err != nil {
		fatal("compile snapshot index", err)
	}
	var publicationDirectory string
	if *allowExistingIdentical {
		publicationDirectory, err = bundle.WriteIdempotent(*outputDirectory)
	} else {
		publicationDirectory, err = bundle.Write(*outputDirectory)
	}
	if err != nil {
		fatal("publish snapshot index", err)
	}
	manifest := bundle.Manifest.DictionaryManifest
	fmt.Printf("publication_dir=%s\nmanifest_digest=%s\ndictionary_digest=%s\nsidecar_digest=%s\ncold_payload_digest=%s\nhot_index_digest=%s\n",
		publicationDirectory, bundle.Manifest.ManifestDigest, manifest.DictionaryDigest, manifest.SidecarDigest,
		manifest.ColdPayloadDigest, manifest.HotIndexDigest)
}

func fatal(operation string, err error) {
	fmt.Fprintf(os.Stderr, "snapshot-index: %s: %v\n", operation, err)
	os.Exit(1)
}
