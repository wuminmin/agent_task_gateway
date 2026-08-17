// Command final-v5-adapter-stderr-scan gates one private Adapter stderr file
// with the existing Final-V5 credential scanner before campaign retention.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

type envNames []string

func (values *envNames) String() string { return strings.Join(*values, ",") }
func (values *envNames) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("sensitive environment name is empty")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	flags := flag.NewFlagSet("final-v5-adapter-stderr-scan", flag.ContinueOnError)
	input := flags.String("input", "", "private Adapter stderr file")
	output := flags.String("output", "", "create-exclusive credential scan report")
	var names envNames
	var sensitiveJSONFiles envNames
	flags.Var(&names, "sensitive-env", "environment variable whose exact non-empty value is forbidden; repeatable")
	flags.Var(&sensitiveJSONFiles, "sensitive-json-file", "private JSON whose non-empty string values are forbidden; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 || *input == "" || *output == "" {
		os.Exit(2)
	}
	value, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-adapter-stderr-scan: read input")
		os.Exit(1)
	}
	seen := map[string]bool{}
	var sensitive []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			if value := os.Getenv(name); value != "" {
				sensitive = append(sensitive, value)
			}
		}
	}
	for _, path := range sensitiveJSONFiles {
		values, readErr := readSensitiveJSONValues(path)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "final-v5-adapter-stderr-scan: read sensitive JSON")
			os.Exit(1)
		}
		sensitive = append(sensitive, values...)
	}
	sort.Strings(sensitive)
	report, err := finalv5publication.ValidateAdapterStderr(value, sensitive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-adapter-stderr-scan:", err)
		os.Exit(1)
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		var file *os.File
		file, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = file.Write(append(payload, '\n'))
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-adapter-stderr-scan: write report")
		os.Exit(1)
	}
}

func readSensitiveJSONValues(path string) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("sensitive JSON must be a private regular file no larger than 1 MiB")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("decode sensitive JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("sensitive JSON has trailing content")
	}
	var values []string
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				values = append(values, typed)
			}
		case []any:
			for _, element := range typed {
				visit(element)
			}
		case map[string]any:
			for _, element := range typed {
				visit(element)
			}
		}
	}
	visit(document)
	return values, nil
}
