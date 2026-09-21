package s3backend

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/minio/minio-go/v7"
)

// The S3 error codes this backend reasons about. They are the store's own
// words and are compared rather than parsed out of a message, because a
// message is prose and a code is an API.
const (
	// codeNotImplemented is how Backblaze B2 refuses a conditional write:
	// "A header you provided implies functionality that is not
	// implemented". Measured against the real service on 2026-09-17.
	codeNotImplemented = "NotImplemented"
	// codePreconditionFailed is a conditional write refused because the
	// object is already there, which is the whole mechanism this backend
	// locks with. It is a SUCCESS for the probe and a lock conflict for
	// Lock.
	codePreconditionFailed = "PreconditionFailed"
	// codeNoSuchKey is nothing stored, which for Get is the ordinary state
	// of a project that has never applied.
	codeNoSuchKey = "NoSuchKey"
)

// storeError is an error the STORE reported, carrying the store's own code and
// message.
//
// It exists so the rest of this package can branch on a code without importing
// minio's error shape everywhere, and so a refusal can QUOTE the store. B2's
// explanation of why it cannot do a conditional write is better than anything
// this plugin would invent, and a user reading it learns something actionable
// about their store rather than about this plugin's opinion of it.
type storeError struct {
	Code    string
	Message string
}

func (e storeError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message + ` (error code: "` + e.Code + `")`
}

// codeOf reports the store's error code, or "" for anything that did not come
// from the store — a timeout, a DNS failure, a closed connection. Those are
// deliberately not codes: a transport failure must never be mistaken for a
// store that answered.
func codeOf(err error) string {
	var se storeError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// objectStore is the slice of an S3 client this backend uses, and nothing
// more.
//
// It is an interface for one reason: the two failure modes the probe exists to
// catch cannot be produced by a real store. MinIO always honours
// If-None-Match, so the refusal paths are only reachable through a double.
// Keeping the surface to four methods means the double is a few lines rather
// than a second S3 implementation with its own bugs.
type objectStore interface {
	// put writes an object whole. When ifAbsent is set the write is
	// CONDITIONAL on nothing being there (If-None-Match: *), and a store
	// that already holds the key must refuse with PreconditionFailed.
	put(ctx context.Context, key string, body []byte, ifAbsent bool) error
	// get reads an object whole, reporting NoSuchKey when there is none.
	get(ctx context.Context, key string) ([]byte, error)
	// remove deletes an object. Deleting one that is not there succeeds,
	// as it does in S3.
	remove(ctx context.Context, key string) error
	// list names every key under a prefix, sorted.
	list(ctx context.Context, prefix string) ([]string, error)
}

// minioStore is objectStore over a real bucket.
type minioStore struct {
	client *minio.Client
	bucket string
}

// newObjectStore is what Configure uses in production.
func newObjectStore(c Config) (objectStore, error) {
	client, err := newClient(c)
	if err != nil {
		return nil, err
	}
	return minioStore{client: client, bucket: c.Bucket}, nil
}

func (s minioStore) put(ctx context.Context, key string, body []byte, ifAbsent bool) error {
	opts := minio.PutObjectOptions{}
	if ifAbsent {
		// The header the whole locking design rests on. Task 3's probe
		// proves the store honours it before any of this is relied on.
		opts.SetMatchETagExcept("*")
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(body), int64(len(body)), opts)
	return asStoreError(err)
}

func (s minioStore) get(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, asStoreError(err)
	}
	defer object.Close()
	// minio's GetObject is lazy: a missing key surfaces here, on the read,
	// not on the call above.
	body, err := io.ReadAll(object)
	if err != nil {
		return nil, asStoreError(err)
	}
	return body, nil
}

func (s minioStore) remove(ctx context.Context, key string) error {
	return asStoreError(s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}))
}

func (s minioStore) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if info.Err != nil {
			return nil, asStoreError(info.Err)
		}
		keys = append(keys, info.Key)
	}
	// ListObjects yields keys in lexical order already; sorting is List's
	// job once the keys have become environment names.
	return keys, nil
}

// asStoreError turns what minio returns into a code this package can branch
// on, and leaves anything that is not a store response alone.
func asStoreError(err error) error {
	if err == nil {
		return nil
	}
	// errors.As rather than minio.ToErrorResponse, which only unwraps an
	// ErrorResponse it is handed directly and returns an empty one for a
	// wrapped error - which would read as "the store said nothing".
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Code != "" {
		return storeError{Code: resp.Code, Message: resp.Error()}
	}
	return err
}
