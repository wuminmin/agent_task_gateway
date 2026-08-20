package main

import "testing"

func TestParseProcStatusExtractsReadOnlyOAMetrics(t *testing.T) {
	values, err := parseProcStatus([]byte("Name:\toa-demo\nVmSize:\t12000 kB\nVmRSS:\t3456 kB\nThreads:\t7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if values["VmRSS"] != 3456 || values["VmSize"] != 12000 || values["Threads"] != 7 {
		t.Fatalf("process status metrics = %+v", values)
	}
}
