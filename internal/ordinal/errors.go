// Package ordinal implements the exact, compressed representation used by
// TaskGate exposure V4.  It changes how facts are represented, never what a
// canonical exposure.FactID means.
package ordinal

import "errors"

var (
	ErrInvalid              = errors.New("invalid ordinal value")
	ErrDigestMismatch       = errors.New("ordinal content digest mismatch")
	ErrNonCanonical         = errors.New("non-canonical ordinal encoding")
	ErrUnknownFact          = errors.New("unknown ordinal fact")
	ErrFactCollision        = errors.New("ordinal fact hash collision")
	ErrMultiplicityOverflow = errors.New("ordinal witness multiplicity overflow")
)
