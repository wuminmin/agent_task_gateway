package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

type prepareOptions struct {
	InputDirectory       string
	ApprovedDirectory    string
	ArtifactDirectory    string
	CalibrationDirectory string
	ManifestPath         string
}

type runOptions struct {
	InputDirectory      string
	ArtifactDirectory   string
	CatalogDirectory    string
	DatasetManifestPath string
	GeneratorPath       string
	ConfigPath          string
	OutputPath          string
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rq5-online-transition:", err)
		os.Exit(1)
	}
}

func execute(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("subcommand prepare or run is required")
	}
	switch arguments[0] {
	case "prepare":
		set := flag.NewFlagSet("prepare", flag.ContinueOnError)
		set.SetOutput(os.Stderr)
		var options prepareOptions
		set.StringVar(&options.InputDirectory, "input-dir", "", "candidate compiler input directory")
		set.StringVar(&options.ApprovedDirectory, "approved-dir", "", "new approved compiler input directory")
		set.StringVar(&options.ArtifactDirectory, "artifact-dir", "", "new verified publication artifact root")
		set.StringVar(&options.CalibrationDirectory, "calibration-dir", "", "new calibration artifact root")
		set.StringVar(&options.ManifestPath, "manifest", "", "new preparation manifest path")
		if err := set.Parse(arguments[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("prepare accepts no positional arguments")
		}
		return preparePublications(options)
	case "run":
		set := flag.NewFlagSet("run", flag.ContinueOnError)
		set.SetOutput(os.Stderr)
		var options runOptions
		set.StringVar(&options.InputDirectory, "input-dir", "", "approved compiler input directory")
		set.StringVar(&options.ArtifactDirectory, "artifact-dir", "", "verified publication artifact root")
		set.StringVar(&options.CatalogDirectory, "catalog-dir", "", "new generated Catalog artifact directory")
		set.StringVar(&options.DatasetManifestPath, "dataset-manifest", "", "deterministic live dataset manifest JSON")
		set.StringVar(&options.GeneratorPath, "generator", "", "daily fixture generator source bound by SHA-256")
		set.StringVar(&options.ConfigPath, "config", "", "daily experiment config bound by SHA-256")
		set.StringVar(&options.OutputPath, "output", "", "new online evidence JSON path")
		if err := set.Parse(arguments[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("run accepts no positional arguments")
		}
		return runOnlineExperiment(options)
	default:
		return fmt.Errorf("unknown subcommand %q", strings.TrimSpace(arguments[0]))
	}
}
