package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
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

type finalV5CycleOptions struct {
	RequestPath              string
	InputDirectory           string
	FixtureArtifactDirectory string
	TargetArtifactDirectory  string
	PhaseDirectory           string
	DatasetManifestPath      string
	GeneratorPath            string
	ConfigPath               string
	OutputPath               string
}

var (
	prepareCommand      = preparePublications
	legacyRunCommand    = runOnlineExperiment
	finalV5CycleCommand = runFinalV5Cycle
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rq5-online-transition:", err)
		os.Exit(1)
	}
}

func execute(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("subcommand prepare, run, or final-v5-cycle is required")
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
		return prepareCommand(options)
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
		return legacyRunCommand(options)
	case "final-v5-cycle":
		set := flag.NewFlagSet("final-v5-cycle", flag.ContinueOnError)
		set.SetOutput(os.Stderr)
		var options finalV5CycleOptions
		set.StringVar(&options.RequestPath, "request", "", "source-driver request JSON")
		set.StringVar(&options.InputDirectory, "input-dir", "", "approved compiler input directory")
		set.StringVar(&options.FixtureArtifactDirectory, "fixture-artifact-dir", "", "shared verified publication artifact root")
		set.StringVar(&options.TargetArtifactDirectory, "target-artifact-dir", "", "current measured target artifact root")
		set.StringVar(&options.PhaseDirectory, "phase-dir", "", "current measured phase report directory")
		set.StringVar(&options.DatasetManifestPath, "dataset-manifest", "", "deterministic live dataset manifest JSON")
		set.StringVar(&options.GeneratorPath, "generator", "", "daily fixture generator source")
		set.StringVar(&options.ConfigPath, "config", "", "daily experiment config")
		set.StringVar(&options.OutputPath, "output", "", "new strict driver-response JSON")
		if err := set.Parse(arguments[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("final-v5-cycle accepts no positional arguments")
		}
		return finalV5CycleCommand(options)
	case "healthcheck":
		set := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		set.SetOutput(os.Stderr)
		url := set.String("url", "", "exact local OA readiness URL")
		if err := set.Parse(arguments[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("healthcheck accepts no positional arguments")
		}
		return runLocalOAHealthcheck(*url)
	default:
		return fmt.Errorf("unknown subcommand %q", strings.TrimSpace(arguments[0]))
	}
}

func runLocalOAHealthcheck(url string) error {
	if url != "http://127.0.0.1:8092/health/ready" {
		return errors.New("healthcheck URL is outside the fixed local OA readiness endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return probeLocalOAReadiness(ctx, http.DefaultClient, url)
}

type localOAHealthClient interface {
	Do(*http.Request) (*http.Response, error)
}

func probeLocalOAReadiness(ctx context.Context, client localOAHealthClient, url string) error {
	if client == nil {
		return errors.New("OA readiness HTTP client is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	if response == nil {
		return errors.New("OA readiness returned no HTTP response")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	// oa-demo's readiness endpoint intentionally returns 204 with an empty
	// body. Accept that exact contract; other 2xx statuses must not silently
	// weaken the Compose health gate.
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("OA readiness status = %d", response.StatusCode)
	}
	return nil
}
