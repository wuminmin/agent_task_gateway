package experiment

import "taskbound.local/agent-data-gateway/internal/queryreceipt"

// currentReceiptVersion keeps evaluation gates on the only Receipt version
// the production build reads and writes. Historical sample decoding remains a
// separate wire-compatibility concern and must not supply this live-run pin.
func currentReceiptVersion(version string) bool {
	return version == queryreceipt.Version
}
