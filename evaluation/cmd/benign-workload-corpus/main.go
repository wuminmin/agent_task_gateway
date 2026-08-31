// Command benign-workload-corpus derives the frozen (c3) benign-trace corpus
// from the unedited agent-written statements, the frozen lowerability report,
// the live Catalog policy surface, and the closed-form dataset models.
package main

import (
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
)

func main() {
	workload := flag.String("agent-workload", "evaluation/agentworkload", "agent workload directory")
	liveCatalog := flag.String("catalog", "config/catalog.yaml", "live catalog path")
	out := flag.String("out", "evaluation/finalv5benign/corpus-v1.json", "corpus output path")
	flag.Parse()
	manifest, err := finalv5benign.BuildManifest(finalv5benign.BuildInput{
		AgentWorkloadDir: *workload, LiveCatalogPath: *liveCatalog})
	if err != nil {
		fmt.Fprintln(os.Stderr, "benign-workload-corpus:", err)
		os.Exit(1)
	}
	encoded, err := finalv5benign.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benign-workload-corpus:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "benign-workload-corpus:", err)
		os.Exit(1)
	}
	fmt.Printf("statements=%d authorized=%d policy_refused=%d zero_release=%d\n",
		len(manifest.Statements), manifest.AuthorizedStatements, manifest.PolicyRefused, manifest.ZeroRelease)
	fmt.Printf("total_release=%d max_dependency=%d trace_union=%d\n",
		manifest.TotalReleaseFacts, manifest.MaxDependencyFacts, manifest.TraceUnionDependencyFacts)
	for _, budget := range manifest.Budgets {
		fmt.Printf("budget %s: R=%d D=%d O=%d Q=%d\n", budget.Name,
			budget.MaxReleaseFacts, budget.MaxInfluence, budget.MaxOutcome, budget.MaxQueries)
	}
}
