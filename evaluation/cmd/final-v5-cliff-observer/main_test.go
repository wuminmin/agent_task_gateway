package main

import (
	"strings"
	"testing"
)

func TestParseProcStatusExtractsReadOnlyOAMetrics(t *testing.T) {
	values, err := parseProcStatus([]byte("Name:\toa-demo\nVmSize:\t12000 kB\nVmRSS:\t3456 kB\nThreads:\t7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if values["VmRSS"] != 3456 || values["VmSize"] != 12000 || values["Threads"] != 7 {
		t.Fatalf("process status metrics = %+v", values)
	}
}

func TestTableStatsQueryNormalizesInitialNullCounters(t *testing.T) {
	for _, counter := range []string{"n_live_tup", "seq_scan", "seq_tup_read", "idx_scan", "idx_tup_fetch"} {
		if !strings.Contains(tableStatsSQL, "COALESCE("+counter+", 0)") {
			t.Fatalf("table stats query does not normalize %s", counter)
		}
	}
}
