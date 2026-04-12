package store

import (
	"io"
	"time"
)

// Object is the common metadata type returned by any Source listing.
type Object struct {
	Key          string
	ETag         string
	Size         int64
	LastModified time.Time
}

// Source is implemented by any object-storage backend that can be read from.
// pageToken is backend-specific: an OSS Marker, an S3 ContinuationToken, etc.
type Source interface {
	// ListPage returns one page of objects under prefix starting at pageToken.
	// Returns (objects, nextPageToken, isTruncated, err).
	ListPage(prefix, pageToken string, pageSize int) ([]Object, string, bool, error)

	// GetObjectStream returns a streaming reader for key.
	// The caller must close the returned ReadCloser.
	GetObjectStream(key string) (io.ReadCloser, error)
}

// Destination is implemented by any object-storage backend that can be written to.
type Destination interface {
	// PutObjectFromStream uploads body to key.
	// size must be the exact content length; pass -1 if unknown (forces chunked).
	PutObjectFromStream(key string, body io.Reader, size int64) error

	// Close releases any SDK-level resources.
	Close()
}
