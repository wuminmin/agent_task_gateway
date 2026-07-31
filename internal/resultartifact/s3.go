package resultartifact

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Config struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
}

type S3Backend struct {
	client *minio.Client
	bucket string
}

func NewS3Backend(config S3Config) (*S3Backend, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	bucket := strings.TrimSpace(config.Bucket)
	accessKey := strings.TrimSpace(config.AccessKey)
	secretKey := strings.TrimSpace(config.SecretKey)
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("object-store endpoint, bucket, access key, and secret key are required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "" {
		return nil, fmt.Errorf("invalid object-store endpoint")
	}
	lookup := minio.BucketLookupAuto
	if config.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(parsed.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       parsed.Scheme == "https",
		Region:       strings.TrimSpace(config.Region),
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("create object-store client: %w", err)
	}
	return &S3Backend{client: client, bucket: bucket}, nil
}

func (backend *S3Backend) Put(ctx context.Context, key string, body io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	if backend == nil || backend.client == nil || strings.TrimSpace(key) == "" || body == nil || size < 0 {
		return ObjectInfo{}, errors.New("invalid object put")
	}
	upload, err := backend.client.PutObject(ctx, backend.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: options.ContentType, UserMetadata: cloneMetadata(options.Metadata),
	})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("put result object: %w", err)
	}
	return ObjectInfo{Key: key, Size: upload.Size, ETag: upload.ETag, Metadata: cloneMetadata(options.Metadata)}, nil
}

func (backend *S3Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if backend == nil || backend.client == nil || strings.TrimSpace(key) == "" {
		return nil, errors.New("invalid object get")
	}
	object, err := backend.client.GetObject(ctx, backend.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get result object: %w", err)
	}
	// GetObject is lazy. Stat forces authorization and existence checks before a
	// reader is handed to the decryption layer.
	if _, err := object.Stat(); err != nil {
		_ = object.Close()
		return nil, classifyS3ObjectError("stat result object", err)
	}
	return object, nil
}

func (backend *S3Backend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if backend == nil || backend.client == nil || strings.TrimSpace(key) == "" {
		return ObjectInfo{}, errors.New("invalid object stat")
	}
	stat, err := backend.client.StatObject(ctx, backend.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, classifyS3ObjectError("stat result object", err)
	}
	metadata := make(map[string]string, len(stat.UserMetadata))
	for key, value := range stat.UserMetadata {
		if len(value) != 0 {
			metadata[strings.ToLower(key)] = string(value)
		}
	}
	return ObjectInfo{Key: key, Size: stat.Size, ETag: stat.ETag, Metadata: metadata, LastModified: stat.LastModified}, nil
}

func (backend *S3Backend) Copy(ctx context.Context, source, destination, expectedSHA256 string) (ObjectInfo, error) {
	if backend == nil || backend.client == nil || strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return ObjectInfo{}, errors.New("invalid object copy")
	}
	expectedDigest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return ObjectInfo{}, errors.New("invalid expected object digest")
	}
	// CopyObject has no portable destination If-None-Match control. Stream the
	// already encrypted bytes through an explicit conditional multipart commit,
	// so two Gateway instances can never replace a canonical result key.
	sourceInfo, err := backend.Stat(ctx, source)
	if err != nil {
		return ObjectInfo{}, err
	}
	getOptions := minio.GetObjectOptions{}
	if err := getOptions.SetMatchETag(sourceInfo.ETag); err != nil {
		return ObjectInfo{}, fmt.Errorf("bind staging object ETag: %w", err)
	}
	body, err := backend.client.GetObject(ctx, backend.bucket, source, getOptions)
	if err != nil {
		return ObjectInfo{}, classifyS3ObjectError("get staging result object", err)
	}
	defer body.Close()
	if _, err := body.Stat(); err != nil {
		return ObjectInfo{}, classifyS3ObjectError("verify staging result object", err)
	}
	options := minio.PutObjectOptions{
		ContentType:  objectContentType,
		UserMetadata: cloneMetadata(sourceInfo.Metadata),
	}
	options.SetMatchETagExcept("*")
	upload, err := putObjectCreateOnly(ctx, backend.client, backend.bucket, destination, body,
		sourceInfo.Size, expectedDigest, options)
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.Code == minio.PreconditionFailed || response.StatusCode == http.StatusPreconditionFailed {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrObjectAlreadyExists, destination)
		}
		return ObjectInfo{}, fmt.Errorf("promote result object: %w", err)
	}
	stat, err := backend.Stat(ctx, destination)
	if err != nil {
		return ObjectInfo{}, err
	}
	stat.ETag = upload.ETag
	return stat, nil
}

const (
	s3MaximumPartSize   int64 = 5 << 30
	s3MultipartPartSize int64 = 64 << 20
	s3MaximumParts            = 10_000
)

// putObjectCreateOnly always uses explicit multipart upload, including for a
// one-part small object. This lets TaskGate authenticate the full ciphertext
// before the conditional CompleteMultipartUpload creates the canonical key.
// minio.Client.PutObject cannot provide that boundary and currently drops
// custom headers when it internally rebuilds multipart options.
func putObjectCreateOnly(ctx context.Context, client *minio.Client, bucket, key string,
	body io.Reader, size int64, expectedDigest []byte, options minio.PutObjectOptions) (minio.UploadInfo, error) {
	core := minio.Core{Client: client}
	partSize := s3MultipartPartSize
	if needed := (size + s3MaximumParts - 1) / s3MaximumParts; needed > partSize {
		const mib = int64(1 << 20)
		partSize = ((needed + mib - 1) / mib) * mib
	}
	if partSize > s3MaximumPartSize {
		return minio.UploadInfo{}, errors.New("result object exceeds S3 multipart limits")
	}
	uploadID, err := core.NewMultipartUpload(ctx, bucket, key, options)
	if err != nil {
		return minio.UploadInfo{}, err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = core.AbortMultipartUpload(abortCtx, bucket, key, uploadID)
	}()
	parts := make([]minio.CompletePart, 0, int((size+partSize-1)/partSize))
	digest := sha256.New()
	hashedBody := io.TeeReader(body, digest)
	remaining := size
	for partNumber := 1; remaining > 0; partNumber++ {
		length := partSize
		if remaining < length {
			length = remaining
		}
		part, partErr := core.PutObjectPart(ctx, bucket, key, uploadID, partNumber,
			io.LimitReader(hashedBody, length), length, minio.PutObjectPartOptions{})
		if partErr != nil {
			return minio.UploadInfo{}, partErr
		}
		parts = append(parts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
		remaining -= length
	}
	extra, err := io.ReadAll(io.LimitReader(hashedBody, 1))
	if err != nil {
		return minio.UploadInfo{}, err
	}
	if len(extra) != 0 || subtle.ConstantTimeCompare(digest.Sum(nil), expectedDigest) != 1 {
		return minio.UploadInfo{}, errors.New("staging result object digest or size differs from committed evidence")
	}
	upload, err := core.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts, options)
	if err != nil {
		return minio.UploadInfo{}, err
	}
	completed = true
	upload.Size = size
	return upload, nil
}

func (backend *S3Backend) Delete(ctx context.Context, key string) error {
	if backend == nil || backend.client == nil || strings.TrimSpace(key) == "" {
		return errors.New("invalid object delete")
	}
	if err := backend.client.RemoveObject(ctx, backend.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete result object: %w", err)
	}
	return nil
}

func (backend *S3Backend) List(ctx context.Context, prefix, startAfter string, limit int) ([]ObjectInfo, error) {
	if backend == nil || backend.client == nil || strings.TrimSpace(prefix) == "" {
		return nil, errors.New("invalid object list")
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	objects := make([]ObjectInfo, 0, limit)
	stopped := false
	for object := range backend.client.ListObjects(listCtx, backend.bucket, minio.ListObjectsOptions{
		Prefix: prefix, Recursive: true, StartAfter: startAfter, MaxKeys: limit,
	}) {
		if object.Err != nil {
			if stopped {
				continue
			}
			return nil, fmt.Errorf("list result objects: %w", object.Err)
		}
		objects = append(objects, ObjectInfo{
			Key: object.Key, Size: object.Size, ETag: object.ETag, LastModified: object.LastModified,
		})
		if len(objects) == limit {
			stopped = true
			cancel()
		}
	}
	return objects, nil
}

func (backend *S3Backend) PurgeIncompleteUploadsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if backend == nil || backend.client == nil || cutoff.IsZero() {
		return 0, errors.New("invalid incomplete-upload cleanup")
	}
	core := minio.Core{Client: backend.client}
	keyMarker := ""
	uploadIDMarker := ""
	purged := 0
	var failures []error
	for {
		page, err := core.ListMultipartUploads(ctx, backend.bucket, "", keyMarker, uploadIDMarker, "", 1000)
		if err != nil {
			return purged, fmt.Errorf("list incomplete result uploads: %w", err)
		}
		for _, upload := range page.Uploads {
			if upload.Initiated.IsZero() || !upload.Initiated.Before(cutoff) {
				continue
			}
			if err := core.AbortMultipartUpload(ctx, backend.bucket, upload.Key, upload.UploadID); err != nil {
				failures = append(failures, fmt.Errorf("abort incomplete result upload %s: %w", upload.Key, err))
				continue
			}
			purged++
		}
		if !page.IsTruncated {
			break
		}
		if page.NextKeyMarker == keyMarker && page.NextUploadIDMarker == uploadIDMarker {
			failures = append(failures, errors.New("incomplete result upload listing did not advance"))
			break
		}
		keyMarker = page.NextKeyMarker
		uploadIDMarker = page.NextUploadIDMarker
	}
	return purged, errors.Join(failures...)
}

func (backend *S3Backend) Ready(ctx context.Context) error {
	if backend == nil || backend.client == nil {
		return errors.New("object store is unavailable")
	}
	exists, err := backend.client.BucketExists(ctx, backend.bucket)
	if err != nil {
		return fmt.Errorf("check result bucket: %w", err)
	}
	if !exists {
		return errors.New("result bucket does not exist")
	}
	versioning, err := backend.client.GetBucketVersioning(ctx, backend.bucket)
	if err != nil {
		return fmt.Errorf("check result bucket versioning: %w", err)
	}
	if versioning.Enabled() || versioning.Suspended() {
		return errors.New("result bucket versioning must be disabled so retention deletes object bytes")
	}
	return nil
}

func classifyS3ObjectError(operation string, err error) error {
	response := minio.ToErrorResponse(err)
	if response.Code == minio.NoSuchKey || response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", operation, ErrObjectNotFound)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func cloneMetadata(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
