package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type telemetryBefore struct {
	businessLSN lsnSnapshot
	controlLSN  lsnSnapshot
	observer    *observerSnapshot
	observerErr error
}

type lsnSnapshot struct {
	value string
	err   error
}

type observerRunner struct {
	config           observerConfig
	executableSHA256 string
}

func newObserverRunner(cfg *observerConfig) (*observerRunner, error) {
	if cfg == nil {
		return nil, nil
	}
	path, err := exec.LookPath(cfg.Argv[0])
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("observer executable is not a regular executable: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	copy := *cfg
	copy.Argv = append([]string(nil), cfg.Argv...)
	copy.Argv[0] = path
	return &observerRunner{config: copy, executableSHA256: sha256Hex(raw)}, nil
}

func (c *campaign) beforeTelemetry(ctx context.Context, caseID, phase string, trial int) telemetryBefore {
	result := telemetryBefore{
		businessLSN: readLSN(ctx, c.business),
		controlLSN:  readLSN(ctx, c.control),
	}
	if c.observer != nil {
		value, err := c.observer.snapshot(ctx, caseID, phase, trial, "before")
		result.observer, result.observerErr = value, err
	}
	return result
}

func (c *campaign) afterTelemetry(ctx context.Context, before telemetryBefore, caseID, phase string,
	trial int) (walMeasurement, observerDelta) {
	businessAfter := readLSN(ctx, c.business)
	controlAfter := readLSN(ctx, c.control)
	wal := walMeasurement{Status: "measured"}
	businessBytes, errBusiness := lsnDiff(ctx, c.business, before.businessLSN, businessAfter)
	controlBytes, errControl := lsnDiff(ctx, c.control, before.controlLSN, controlAfter)
	if errBusiness != nil || errControl != nil {
		wal.Status = "unmeasured"
		wal.Reason = joinErrors(errBusiness, errControl)
	} else {
		wal.BusinessBytes, wal.ControlBytes = &businessBytes, &controlBytes
	}
	if c.observer == nil {
		return wal, observerDelta{Status: "unmeasured", Reason: "observer was not configured"}
	}
	after, afterErr := c.observer.snapshot(ctx, caseID, phase, trial, "after")
	if before.observerErr != nil || afterErr != nil || before.observer == nil || after == nil {
		return wal, observerDelta{Status: "unmeasured", Reason: joinErrors(before.observerErr, afterErr)}
	}
	result := observerDelta{Status: "measured", Before: before.observer.Metrics, After: after.Metrics,
		MemoryScope: after.MemoryScope, Delta: make(map[string]int64)}
	if before.observer.MemoryScope != after.MemoryScope {
		result.Status = "unmeasured"
		result.Reason = "observer memory_scope changed during the operation"
		return wal, result
	}
	for name, afterValue := range after.Metrics {
		beforeValue, ok := before.observer.Metrics[name]
		if !ok || afterValue < beforeValue {
			continue
		}
		result.Delta[name] = afterValue - beforeValue
	}
	return wal, result
}

func readLSN(ctx context.Context, pool *pgxpool.Pool) lsnSnapshot {
	var value string
	err := pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&value)
	return lsnSnapshot{value: value, err: err}
}

func lsnDiff(ctx context.Context, pool *pgxpool.Pool, before, after lsnSnapshot) (int64, error) {
	if before.err != nil {
		return 0, before.err
	}
	if after.err != nil {
		return 0, after.err
	}
	var value int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::bigint`, after.value, before.value).Scan(&value); err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, errors.New("WAL counter moved backwards")
	}
	return value, nil
}

func (o *observerRunner) snapshot(ctx context.Context, caseID, phase string, trial int, point string) (*observerSnapshot, error) {
	if err := verifyExecutableDigest(o.config.Argv[0], o.executableSHA256); err != nil {
		return nil, err
	}
	timeout := o.config.TimeoutMS
	if timeout == 0 {
		timeout = 5000
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(commandCtx, o.config.Argv[0], o.config.Argv[1:]...)
	command.Env = append(os.Environ(),
		"V4_EVAL_CASE="+caseID,
		"V4_EVAL_PHASE="+phase,
		"V4_EVAL_TRIAL="+strconv.Itoa(trial),
		"V4_EVAL_POINT="+point,
	)
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = 1<<20, 1<<20
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("observer %s: %w: %s", point, err, strings.TrimSpace(stderr.String()))
	}
	var result observerSnapshot
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode observer %s: %w", point, err)
	}
	if result.SchemaVersion != 1 || result.Metrics == nil {
		return nil, fmt.Errorf("observer %s returned an invalid schema", point)
	}
	for name, value := range result.Metrics {
		if strings.TrimSpace(name) == "" || value < 0 {
			return nil, fmt.Errorf("observer %s returned an invalid metric", point)
		}
	}
	return &result, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}

func joinErrors(values ...error) string {
	var messages []string
	for _, value := range values {
		if value != nil {
			messages = append(messages, value.Error())
		}
	}
	if len(messages) == 0 {
		return "measurement unavailable"
	}
	return strings.Join(messages, "; ")
}

func measureCommandPhase(ctx context.Context, name string, metric *commandMetric, outputRoot string) phaseMeasurement {
	if metric == nil {
		return phaseMeasurement{Status: "unmeasured", Reason: name + " command was not configured"}
	}
	runs := metric.Runs
	if runs == 0 {
		runs = 1
	}
	result := phaseMeasurement{Status: "measured"}
	for run := 1; run <= runs; run++ {
		runDir := filepath.Join(outputRoot, fmt.Sprintf("run-%d-%d", run, time.Now().UnixNano()))
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			result.Status = "failed"
			result.Runs = append(result.Runs, commandRun{Run: run, Status: "failed", Error: err.Error()})
			continue
		}
		result.Runs = append(result.Runs, measureCommand(ctx, *metric, run, runDir))
		if result.Runs[len(result.Runs)-1].Status != "measured" {
			result.Status = "failed"
		}
	}
	return result
}

func measureCommand(ctx context.Context, metric commandMetric, run int, runDir string) commandRun {
	result := commandRun{Run: run, Status: "measured"}
	argv := substituteRunDir(metric.Argv, runDir)
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	rawExecutable, err := os.ReadFile(executable)
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	result.ExecutableSHA256 = sha256Hex(rawExecutable)
	argv[0] = executable
	timeout := metric.TimeoutMS
	if timeout == 0 {
		timeout = 15 * 60 * 1000
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "V4_EVAL_RUN_DIR="+runDir, "V4_EVAL_RUN="+strconv.Itoa(run))
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = 4<<20, 4<<20
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	if err := command.Start(); err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	peakDone := make(chan int64, 1)
	go sampleProcessRSS(commandCtx, command.Process.Pid, peakDone)
	err = command.Wait()
	result.WallMS = durationMS(time.Since(started))
	peak := <-peakDone
	if peak > 0 {
		result.RootPeakRSSBytes = &peak
		result.RSSScope = "root_process_proc_status_vmhwm_or_vmrss"
	}
	digest := sha256.New()
	digest.Write(stdout.Bytes())
	digest.Write([]byte{0})
	digest.Write(stderr.Bytes())
	result.OutputSHA256 = hex.EncodeToString(digest.Sum(nil))
	exitCode := 0
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	result.ExitCode = &exitCode
	if len(metric.ArtifactPaths) > 0 {
		value, err := byteSize(substituteRunDir(metric.ArtifactPaths, runDir))
		if err != nil {
			result.Status, result.Error = "failed", "artifact measurement: "+err.Error()
		} else {
			result.ArtifactBytes = &value
		}
	}
	if len(metric.HotPaths) > 0 {
		value, err := byteSize(substituteRunDir(metric.HotPaths, runDir))
		if err != nil {
			result.Status, result.Error = "failed", "hot artifact measurement: "+err.Error()
		} else {
			result.HotArtifactBytes = &value
		}
	}
	return result
}

func verifyExecutableDigest(path, expected string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256Hex(raw) != expected {
		return errors.New("observer executable changed after preflight")
	}
	return nil
}

func sampleProcessRSS(ctx context.Context, pid int, done chan<- int64) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var peak int64
	for {
		value, err := processRSS(pid)
		if err == nil && value > peak {
			peak = value
		}
		select {
		case <-ctx.Done():
			done <- peak
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); errors.Is(err, os.ErrNotExist) {
				done <- peak
				return
			}
		}
	}
}

func processRSS(pid int) (int64, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	var result int64
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") && !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil && value*1024 > result {
			result = value * 1024
		}
	}
	if result == 0 {
		return 0, errors.New("process status omitted VmRSS/VmHWM")
	}
	return result, nil
}

func substituteRunDir(values []string, runDir string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.ReplaceAll(value, "{{run_dir}}", runDir)
	}
	return result
}

func measureArtifacts(totalPaths, hotPaths []string) artifactMeasurement {
	if len(totalPaths) == 0 && len(hotPaths) == 0 {
		return artifactMeasurement{Status: "unmeasured", Reason: "artifact paths were not configured"}
	}
	result := artifactMeasurement{Status: "measured"}
	if len(totalPaths) == 0 {
		result.Status = "partial"
		result.Reason = "total artifact paths were not configured"
	} else {
		value, err := byteSize(totalPaths)
		if err != nil {
			return artifactMeasurement{Status: "failed", Reason: err.Error()}
		}
		result.TotalBytes = &value
	}
	if len(hotPaths) == 0 {
		result.Status = "partial"
		if result.Reason == "" {
			result.Reason = "hot artifact paths were not configured"
		}
	} else {
		value, err := byteSize(hotPaths)
		if err != nil {
			return artifactMeasurement{Status: "failed", Reason: err.Error()}
		}
		result.HotBytes = &value
	}
	return result
}

func byteSize(paths []string) (int64, error) {
	seenRoots := make(map[string]struct{})
	seenFiles := make(map[string]struct{})
	var total int64
	for _, root := range paths {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return 0, err
		}
		if _, duplicate := seenRoots[absolute]; duplicate {
			continue
		}
		seenRoots[absolute] = struct{}{}
		info, err := os.Lstat(absolute)
		if err != nil {
			return 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return 0, fmt.Errorf("artifact root is a symlink: %s", root)
		}
		err = filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("artifact tree contains symlink: %s", path)
			}
			if info.Mode().IsRegular() {
				canonical, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				if _, duplicate := seenFiles[canonical]; !duplicate {
					seenFiles[canonical] = struct{}{}
					total += info.Size()
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
