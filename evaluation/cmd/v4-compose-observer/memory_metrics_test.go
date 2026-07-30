package main

import (
	"strings"
	"testing"
)

func TestParseCgroupLimitAcceptsFiniteAndUnlimitedValues(t *testing.T) {
	finite, err := parseCgroupLimit([]byte("1073741824\n"), "memory.max")
	if err != nil || finite != 1_073_741_824 {
		t.Fatalf("finite cgroup limit = %d, err=%v", finite, err)
	}
	unlimited, err := parseCgroupLimit([]byte("max\n"), "memory.max")
	if err != nil || unlimited != unlimitedCgroupValue {
		t.Fatalf("unlimited cgroup limit = %d, err=%v", unlimited, err)
	}
	for _, raw := range []string{"", "-1", "MAX", "1 2", "9223372036854775808"} {
		if _, err := parseCgroupLimit([]byte(raw), "memory.max"); err == nil {
			t.Fatalf("invalid cgroup limit %q was accepted", raw)
		}
	}
}

func TestParseMemoryEventsRequiresUniqueNonnegativeCounters(t *testing.T) {
	got, err := parseMemoryEvents([]byte("low 0\nhigh 3\nmax 7\noom 2\noom_kill 1\noom_group_kill 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got["max"] != 7 || got["oom"] != 2 || got["oom_kill"] != 1 || got["high"] != 3 {
		t.Fatalf("unexpected memory.events values: %#v", got)
	}

	tests := []struct {
		name   string
		raw    string
		reason string
	}{
		{name: "missing max", raw: "oom 0\noom_kill 0\n", reason: `omitted "max"`},
		{name: "missing oom", raw: "max 0\noom_kill 0\n", reason: `omitted "oom"`},
		{name: "missing oom kill", raw: "max 0\noom 0\n", reason: `omitted "oom_kill"`},
		{name: "duplicate", raw: "max 0\nmax 1\noom 0\noom_kill 0\n", reason: `repeats "max"`},
		{name: "negative", raw: "max -1\noom 0\noom_kill 0\n", reason: "not a nonnegative int64"},
		{name: "nonnumeric", raw: "max many\noom 0\noom_kill 0\n", reason: "not a nonnegative int64"},
		{name: "extra field", raw: "max 0 trailing\noom 0\noom_kill 0\n", reason: "malformed row"},
		{name: "unknown malformed", raw: "low unknown\nmax 0\noom 0\noom_kill 0\n", reason: "not a nonnegative int64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMemoryEvents([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("parseMemoryEvents error = %v, want reason containing %q", err, test.reason)
			}
		})
	}
}
