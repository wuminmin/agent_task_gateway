package finalv5oracle

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOracleImportBoundary(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate oracle source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	command := exec.Command("go", "list", "-deps", "./evaluation/finalv5oracle")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list oracle dependencies: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, forbidden := range []string{
			"taskbound.local/agent-data-gateway/internal/exposure",
			"taskbound.local/agent-data-gateway/internal/control",
			"taskbound.local/agent-data-gateway/internal/gateway",
			"taskbound.local/agent-data-gateway/internal/semanticcache",
			"taskbound.local/agent-data-gateway/internal/ordinal",
			"taskbound.local/agent-data-gateway/internal/queryplan",
			"taskbound.local/agent-data-gateway/internal/sqlpolicy",
			"taskbound.local/agent-data-gateway/internal/queryreceipt",
		} {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("independent oracle imports forbidden production dependency %q", dependency)
			}
		}
	}
}
