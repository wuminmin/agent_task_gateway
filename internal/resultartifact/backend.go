package resultartifact

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrObjectNotFound is the only error that permits Promote to conclude that
	// a canonical key is absent. Authorization and transport failures must never
	// be treated as absence because doing so could overwrite immutable evidence.
	ErrObjectNotFound = errors.New("result object not found")
	// ErrObjectAlreadyExists reports a failed create-only canonical write.
	ErrObjectAlreadyExists = errors.New("result object already exists")
	// ErrArtifactIntegrity marks authenticated ciphertext or Parquet structure
	// that cannot match committed evidence. It is deterministic, unlike a
	// transient object-store transport failure.
	ErrArtifactIntegrity = errors.New("result artifact integrity failure")
)

// ObjectInfo is the storage-provider-independent evidence returned for an
// immutable result object. ETag is retained for operations diagnostics only;
// TaskGate integrity checks use the explicit SHA-256 metadata.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	Metadata     map[string]string
	LastModified time.Time
}

type PutOptions struct {
	ContentType string
	Metadata    map[string]string
}

// Backend is the narrow S3-compatible boundary used by the result artifact
// manager. Implementations must keep the bucket private; callers never receive
// object-store credentials or object keys.
type Backend interface {
	Put(context.Context, string, io.Reader, int64, PutOptions) (ObjectInfo, error)
	Get(context.Context, string) (io.ReadCloser, error)
	Stat(context.Context, string) (ObjectInfo, error)
	// Copy creates destination from source without replacing an existing key.
	// Implementations must return ErrObjectAlreadyExists when the create-only
	// precondition loses a race.
	Copy(context.Context, string, string, string) (ObjectInfo, error)
	// List returns at most limit keys in lexical order, strictly after
	// startAfter. It is used only for Control-aware staging garbage collection.
	List(context.Context, string, string, int) ([]ObjectInfo, error)
	Delete(context.Context, string) error
	Ready(context.Context) error
}

// IncompleteUploadCleaner is an optional S3 capability. Multipart uploads are
// never canonical objects, so old fragments can be aborted independently of
// result retention and PENDING intent recovery.
type IncompleteUploadCleaner interface {
	PurgeIncompleteUploadsBefore(context.Context, time.Time) (int, error)
}
