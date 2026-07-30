// Package semanticcache defines the fail-closed identity of a committed V4
// query materialization. It does not decide authorization and it never turns a
// cache miss into a guessed hit.
package semanticcache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	KeyVersion      = "taskgate-semantic-replay-v1"
	CompilerV1      = "taskgate-ordinal-compiler-v1"
	OrderingV1      = "taskgate-canonical-order-v1"
	PaginationV1    = "taskgate-pagination-v1"
	keyDigestDomain = "TASKGATE-SEMANTIC-REPLAY-KEY-V1\x00"
)

var ErrInvalid = errors.New("invalid semantic replay key")

// Binding contains every authority, semantics, snapshot and compiler input
// that can change a committed observation or encrypted result. Fields are
// deliberately scalar and encoded in fixed order, avoiding map/JSON-order
// ambiguity.
type Binding struct {
	Version             string
	TaskID              string
	GrantDigest         string
	AuthorizationDigest string
	TypedNormalForm     string
	PlanDigest          string
	CatalogDigest       string
	SchemaDigest        string
	DictionarySetDigest string
	ExposureProfile     string
	CompilerVersion     string
	OrderingVersion     string
	PaginationVersion   string
	ResultEncoding      string
}

func (b Binding) Normalize() Binding {
	if b.Version == "" {
		b.Version = KeyVersion
	}
	if b.CompilerVersion == "" {
		b.CompilerVersion = CompilerV1
	}
	if b.OrderingVersion == "" {
		b.OrderingVersion = OrderingV1
	}
	if b.PaginationVersion == "" {
		b.PaginationVersion = PaginationV1
	}
	if b.ResultEncoding == "" {
		b.ResultEncoding = "json-v1"
	}
	return b
}

func (b Binding) Validate() error {
	b = b.Normalize()
	if b.Version != KeyVersion || b.CompilerVersion == "" || b.OrderingVersion == "" || b.PaginationVersion == "" {
		return fmt.Errorf("%w: unsupported version binding", ErrInvalid)
	}
	for name, value := range map[string]string{
		"task": b.TaskID, "typed normal form": b.TypedNormalForm,
		"exposure profile": b.ExposureProfile, "result encoding": b.ResultEncoding,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: %s is required", ErrInvalid, name)
		}
	}
	for name, value := range map[string]string{
		"grant": b.GrantDigest, "authorization": b.AuthorizationDigest,
		"plan": b.PlanDigest, "catalog": b.CatalogDigest, "schema": b.SchemaDigest,
		"dictionary set": b.DictionarySetDigest,
	} {
		if !validDigest(value) {
			return fmt.Errorf("%w: %s digest", ErrInvalid, name)
		}
	}
	return nil
}

// Digest returns a domain-separated, deterministic cache key. It is safe to
// index but is not an authorization token; callers must reauthorize every hit.
func (b Binding) Digest() (string, error) {
	b = b.Normalize()
	if err := b.Validate(); err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(keyDigestDomain))
	for _, value := range []string{
		b.Version, b.TaskID, b.GrantDigest, b.AuthorizationDigest,
		b.TypedNormalForm, b.PlanDigest, b.CatalogDigest, b.SchemaDigest,
		b.DictionarySetDigest, b.ExposureProfile, b.CompilerVersion,
		b.OrderingVersion, b.PaginationVersion, b.ResultEncoding,
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
