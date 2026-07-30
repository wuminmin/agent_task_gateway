package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRunCapturesOneJSONCommandReport(t *testing.T) {
	report, err := run(context.Background(), []string{
		"-phase", "build", "-day", "day0", "-sample", "1",
		"/bin/sh", "-c", `printf '{"mode":"build","total_artifact_bytes":7}\n'`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || report.ExitCode != 0 || report.WallMS <= 0 || report.StdoutBytes == 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	var child map[string]any
	if err := json.Unmarshal(report.CommandReport, &child); err != nil || child["mode"] != "build" {
		t.Fatalf("command report = %#v, err = %v", child, err)
	}
}

func TestRunRejectsNonJSONAndNonzeroCommands(t *testing.T) {
	for _, argv := range [][]string{
		{"-phase", "activation", "-day", "day3", "-sample", "1", "/bin/sh", "-c", "printf nope"},
		{"-phase", "activation", "-day", "day3", "-sample", "1", "/bin/sh", "-c", "exit 7"},
	} {
		report, err := run(context.Background(), argv)
		if err == nil || report.Status != "fail" {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	}
}

func TestParseOptionsRejectsInvalidExperimentCoordinates(t *testing.T) {
	if _, err := parseOptions([]string{"-phase", "switch", "-day", "day9", "/bin/true"}); err == nil {
		t.Fatal("invalid phase/day accepted")
	}
}
