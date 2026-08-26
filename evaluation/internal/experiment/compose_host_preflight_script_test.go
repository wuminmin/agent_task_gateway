package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const composeHostPreflightPath = "../../final-v5-wsl2/scripts/compose-host-preflight.sh"

func TestLiveRunnersPreflightHostBeforeCreatingOrBuildingAnything(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		sideEffects []string
	}{
		{
			name: "targeted Artifact",
			path: artifactTargetedLauncherPath,
			sideEffects: []string{
				`mkdir -m 700 -p "$outdir"`,
				`build_sealed ./evaluation/cmd/final-v5-adapter`,
				`go run ./evaluation/cmd/final-v5-gateway-build build`,
				`"${compose[@]}" up`,
			},
		},
		{
			name: "publication campaign",
			path: "../../final-v5-wsl2/scripts/run-deployment.sh",
			sideEffects: []string{
				`mkdir -m 700 -p "$preflight_experiment_root"`,
				`go run "./evaluation/cmd/${commands[$index]}"`,
				`go build -buildvcs=false -trimpath -o "$adapter_tmp"`,
				`go run ./evaluation/cmd/final-v5-gateway-build build`,
				`> "$marker"`,
				`"${compose_build[@]}" build "${ordinary_build_services[@]}"`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bodyBytes, err := os.ReadFile(filepath.Clean(test.path))
			if err != nil {
				t.Fatal(err)
			}
			body := string(bodyBytes)
			preflight := strings.Index(body, `bash evaluation/final-v5-wsl2/scripts/compose-host-preflight.sh`)
			project := strings.Index(body, `deployment-project-name.sh`)
			if project < 0 || preflight <= project {
				t.Fatal("host preflight is not bound to the derived Compose project")
			}
			for _, sideEffect := range test.sideEffects {
				position := strings.Index(body, sideEffect)
				if position < 0 {
					t.Fatalf("runner omits expected side effect %q", sideEffect)
				}
				if preflight > position {
					t.Fatalf("runner reaches %q before the host preflight", sideEffect)
				}
			}
		})
	}
}
