// Package finalv5contractfs carries the frozen Final-V5 author contracts into
// every runtime binary as embedded bytes.
//
// The Contract Index and the machine contracts are the runtime source of
// truth. Embedding them means a runtime cannot be pointed at a different
// contract tree, and it makes the index digest revalidation performed by
// evaluation/finalv5contracts a real check: a contract file edited without a
// matching index update fails closed at load time instead of silently
// changing what an Adapter executes.
//
// Only reviewed, source-controlled contract inputs belong here. Generated
// campaign evidence, private configs, and Dataset Bindings must never be
// embedded.
package finalv5contractfs

import "embed"

// FS holds the reviewed contract inputs rooted at evaluation/final-v5-wsl2.
// Paths inside FS are exactly the paths used by contracts/index-v1.json.
//
//go:embed contracts/index-v1.json
//go:embed contracts/baseline-v1.json contracts/scale-v1.json contracts/artifact-v1.json
//go:embed contracts/benchmark-products-v1.json contracts/oracle-policy-v1.json
//go:embed contracts/profile-activation-v1.json
//go:embed contracts/result-normalization-v1.json
//go:embed catalog/benchmark-contract-v1.yaml
//go:embed sql/contracts
//go:embed sql/datasets/benchmark-v1-generate.sql sql/datasets/benchmark-v1-probe.sql
//go:embed protocol/protocol-v1.yaml protocol/workloads-v1.yaml
//go:embed all:oracle-manifests
var FS embed.FS
