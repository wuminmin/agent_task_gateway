// Package legacyv14 preserves the audited contracts-v1.4 observer wire
// schemas and their historical validation rules for forensic decoding.
//
// The accounting represented here is invalid for publication measurement; see
// docs/final_v5_observer_accounting_v14_audit.md. This package is deliberately
// self-contained and has no live observer launcher. Production packages are
// forbidden from importing it, so decoding an archived v1.4 document cannot
// become a fallback from the v3 runtime.
package legacyv14
