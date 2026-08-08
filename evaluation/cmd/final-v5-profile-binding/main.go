// Command final-v5-profile-binding resolves one cleared deployment profile into
// the exact ProfileBinding the experiment Runner sends to every Adapter
// operation.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-profile-binding:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("final-v5-profile-binding", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := flags.String("registry", "", "source-controlled profile registry")
	alias := flags.String("alias", "", "registered profile alias")
	datasetBindingSHA256 := flags.String("dataset-binding-sha256", "", "SHA-256 of the deployment Dataset Binding")
	out := flags.String("out", "", "create-exclusive ProfileBinding JSON output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	if strings.TrimSpace(*registry) == "" || strings.TrimSpace(*alias) == "" ||
		strings.TrimSpace(*datasetBindingSHA256) == "" || strings.TrimSpace(*out) == "" {
		return errors.New("registry, alias, dataset-binding-sha256 and out are required")
	}

	binding, err := experiment.ResolveProfileBinding(*registry, *alias, *datasetBindingSHA256)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile binding: %w", err)
	}
	return writeExclusive(strings.TrimSpace(*out), append(payload, '\n'))
}

func writeExclusive(path string, payload []byte) error {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create profile binding output: %w", err)
	}
	if _, err := output.Write(payload); err != nil {
		_ = output.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write profile binding output: %w", err)
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close profile binding output: %w", err)
	}
	return nil
}
