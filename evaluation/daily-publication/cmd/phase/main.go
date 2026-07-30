package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const reportSchema = "taskgate-daily-publication-phase-v1"

type options struct {
	phase  string
	day    string
	sample int
	argv   []string
}

type phaseReport struct {
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	Phase         string          `json:"phase"`
	Day           string          `json:"day"`
	Sample        int             `json:"sample"`
	Executable    string          `json:"executable"`
	ArgvSHA256    string          `json:"argv_sha256"`
	WallMS        float64         `json:"wall_ms"`
	PeakRSSBytes  *uint64         `json:"peak_rss_bytes"`
	PeakRSSScope  string          `json:"peak_rss_scope"`
	ExitCode      int             `json:"exit_code"`
	StdoutBytes   int             `json:"stdout_bytes"`
	StdoutSHA256  string          `json:"stdout_sha256"`
	StderrBytes   int             `json:"stderr_bytes"`
	StderrSHA256  string          `json:"stderr_sha256"`
	CommandReport json.RawMessage `json:"command_report,omitempty"`
	Failure       string          `json:"failure,omitempty"`
	Measurement   string          `json:"measurement_boundary"`
}

func main() {
	report, err := run(context.Background(), os.Args[1:])
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encodeErr := encoder.Encode(report)
	if err != nil {
		if encodeErr != nil {
			fmt.Fprintln(os.Stderr, encodeErr)
		}
		os.Exit(1)
	}
	if encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (phaseReport, error) {
	opts, err := parseOptions(args)
	if err != nil {
		return failedReport(options{}, err), err
	}
	encodedArgv, err := json.Marshal(opts.argv)
	if err != nil {
		return failedReport(opts, err), err
	}
	argvDigest := sha256.Sum256(encodedArgv)
	report := phaseReport{
		SchemaVersion: reportSchema,
		Status:        "fail",
		Phase:         opts.phase,
		Day:           opts.day,
		Sample:        opts.sample,
		Executable:    filepath.Base(opts.argv[0]),
		ArgvSHA256:    hex.EncodeToString(argvDigest[:]),
		PeakRSSScope:  "root_process_vm_hwm_linux_procfs",
		ExitCode:      -1,
		Measurement:   "child process wall clock and /proc/<pid>/status VmHWM; excludes container startup and orchestration",
	}

	command := exec.CommandContext(ctx, opts.argv[0], opts.argv[1:]...)
	command.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	started := time.Now()
	if err := command.Start(); err != nil {
		report.WallMS = elapsedMS(started)
		report.Failure = "start child process"
		return report, err
	}
	peakChannel := make(chan uint64, 1)
	stopSampling := make(chan struct{})
	go samplePeakRSS(command.Process.Pid, stopSampling, peakChannel)
	waitErr := command.Wait()
	close(stopSampling)
	peak := <-peakChannel
	report.WallMS = elapsedMS(started)
	if peak > 0 {
		report.PeakRSSBytes = &peak
	}
	report.StdoutBytes = stdout.Len()
	report.StderrBytes = stderr.Len()
	stdoutDigest := sha256.Sum256(stdout.Bytes())
	stderrDigest := sha256.Sum256(stderr.Bytes())
	report.StdoutSHA256 = hex.EncodeToString(stdoutDigest[:])
	report.StderrSHA256 = hex.EncodeToString(stderrDigest[:])
	report.ExitCode = exitCode(waitErr)

	if waitErr != nil {
		report.Failure = "child process returned nonzero"
		return report, waitErr
	}
	commandReport, err := decodeOneJSONObject(stdout.Bytes())
	if err != nil {
		report.Failure = "child stdout was not one JSON object"
		return report, err
	}
	report.CommandReport = commandReport
	report.Status = "pass"
	return report, nil
}

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("daily-publication-phase", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	phase := flags.String("phase", "", "build, strict_verify, or activation")
	day := flags.String("day", "", "day0 through day3")
	sample := flags.Int("sample", 0, "positive measured sample number; zero is calibration")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	argv := flags.Args()
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	validPhase := *phase == "build" || *phase == "strict_verify" || *phase == "activation"
	validDay := *day == "day0" || *day == "day1" || *day == "day2" || *day == "day3"
	if !validPhase || !validDay || *sample < 0 || len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return options{}, errors.New("require -phase build|strict_verify|activation, -day day0..day3, -sample >= 0, and a child argv")
	}
	return options{phase: *phase, day: *day, sample: *sample, argv: append([]string(nil), argv...)}, nil
}

func failedReport(opts options, err error) phaseReport {
	return phaseReport{
		SchemaVersion: reportSchema,
		Status:        "fail",
		Phase:         opts.phase,
		Day:           opts.day,
		Sample:        opts.sample,
		ExitCode:      -1,
		Failure:       err.Error(),
		Measurement:   "child process was not started",
	}
}

func decodeOneJSONObject(payload []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("extra JSON value after command report")
	}
	return json.Marshal(value)
}

func samplePeakRSS(pid int, stop <-chan struct{}, result chan<- uint64) {
	var peak uint64
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	read := func() {
		if value := procStatusBytes(pid, "VmHWM:"); value > peak {
			peak = value
		}
		if value := procStatusBytes(pid, "VmRSS:"); value > peak {
			peak = value
		}
	}
	read()
	for {
		select {
		case <-ticker.C:
			read()
		case <-stop:
			read()
			result <- peak
			return
		}
	}
}

func procStatusBytes(pid int, label string) uint64 {
	payload, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != label {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			return value * 1024
		}
	}
	return 0
}

func elapsedMS(started time.Time) float64 {
	return float64(time.Since(started)) / float64(time.Millisecond)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return -1
}
