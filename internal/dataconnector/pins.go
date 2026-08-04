package dataconnector

import (
	"context"
	"errors"
)

// Session pin statements, as source-controlled constants.
//
// Both pins are transaction-local (`is_local = true`), issued inside the
// governed transaction before any target statement runs. They are exported so
// that an out-of-process observer can derive its control fingerprints from the
// exact bytes the Connector executes, rather than from a re-typed copy that
// could drift.
//
// The two pins are deliberately structurally distinct.
//
// Before contracts v1.5 the representation pin was a second two-call
// `set_config` SELECT with the same parse shape as the safety pin. Because
// pg_stat_statements replaces constants -- including the setting names -- with
// placeholders, both statements normalized to one template and shared a single
// queryid, observed as `calls = 2`. Any statement accounting built on
// normalized templates was therefore unable to tell a safety pin from a
// representation pin, or to detect one being substituted for the other.
//
// The representation pin now carries its own parse structure: a named CTE that
// applies the settings and a projection that reads them back. The read-back is
// not decoration -- the Connector verifies the returned values, so a pin that
// silently failed to apply is caught in the same round trip that applied it.
// This adds no statement, no transaction and no database round trip.
const (
	// SafetySessionPinSQL pins the parsing and name-resolution settings that
	// make an authorized statement mean what the compiler decided it means.
	SafetySessionPinSQL = `SELECT pg_catalog.set_config('search_path', 'pg_catalog', true), pg_catalog.set_config('standard_conforming_strings', 'on', true)`

	// RepresentationPinSQL pins the settings that determine how values are
	// rendered, and returns them for verification.
	RepresentationPinSQL = `WITH taskgate_representation_pin AS (
	SELECT pg_catalog.set_config('TimeZone', 'UTC', true) AS time_zone,
	       pg_catalog.set_config('extra_float_digits', '3', true) AS extra_float_digits
)
SELECT time_zone, extra_float_digits FROM taskgate_representation_pin`
)

// The exact values RepresentationPinSQL must report back. set_config returns the
// value it installed, so a mismatch means the setting did not take effect for
// this transaction and no result derived under it is representable.
const (
	requiredTimeZone         = "UTC"
	requiredExtraFloatDigits = "3"
)

// pinRepresentation applies the representation settings and verifies, in the
// same round trip, that they took effect.
//
// The error deliberately does not name the value the server reported. A pinned
// setting is deployment state, not task data, and the Connector's error surface
// reaches an Agent; reporting the observed value would leak it. The caller only
// needs to know the read is not representable.
func pinRepresentation(ctx context.Context, querier attestationQuerier) error {
	var timeZone, extraFloatDigits string
	if err := querier.QueryRow(ctx, RepresentationPinSQL).Scan(&timeZone, &extraFloatDigits); err != nil {
		return classifyQueryError(err)
	}
	if timeZone != requiredTimeZone || extraFloatDigits != requiredExtraFloatDigits {
		return connectorError(CodeSchemaDrift, errRepresentationNotInForce)
	}
	return nil
}

// errRepresentationNotInForce names the fail-closed condition without carrying
// the observed values, so the message can cross the Connector error surface.
var errRepresentationNotInForce = errors.New("representation settings did not take effect for this transaction")
