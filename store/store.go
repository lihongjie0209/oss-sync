package store

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Object is the common metadata type returned by any Source listing.
type Object struct {
	Key          string
	ETag         string
	Size         int64
	LastModified time.Time
}

// ObjectVisibility is the generic object ACL / canned ACL abstraction shared by providers.
type ObjectVisibility string

const (
	VisibilityUnspecified            ObjectVisibility = ""
	VisibilitySource                 ObjectVisibility = "source"
	VisibilityPrivate                ObjectVisibility = "private"
	VisibilityPublicRead             ObjectVisibility = "public-read"
	VisibilityPublicReadWrite        ObjectVisibility = "public-read-write"
	VisibilityAuthenticatedRead      ObjectVisibility = "authenticated-read"
	VisibilityBucketOwnerRead        ObjectVisibility = "bucket-owner-read"
	VisibilityBucketOwnerFullControl ObjectVisibility = "bucket-owner-full-control"
)

// UploadOptions contains provider-agnostic options for a destination upload.
type UploadOptions struct {
	Visibility ObjectVisibility
}

func NormalizeVisibility(value string) ObjectVisibility {
	return ObjectVisibility(strings.ToLower(strings.TrimSpace(value)))
}

func (v ObjectVisibility) IsExplicit() bool {
	return v != VisibilityUnspecified && v != VisibilitySource
}

func (v ObjectVisibility) IsKnown() bool {
	switch v {
	case VisibilityUnspecified,
		VisibilitySource,
		VisibilityPrivate,
		VisibilityPublicRead,
		VisibilityPublicReadWrite,
		VisibilityAuthenticatedRead,
		VisibilityBucketOwnerRead,
		VisibilityBucketOwnerFullControl:
		return true
	default:
		return false
	}
}

func SupportedDestinationVisibilities(provider string) []ObjectVisibility {
	switch provider {
	case "oss":
		return []ObjectVisibility{
			VisibilityPrivate,
			VisibilityPublicRead,
			VisibilityPublicReadWrite,
		}
	case "obs", "s3":
		return []ObjectVisibility{
			VisibilityPrivate,
			VisibilityPublicRead,
			VisibilityPublicReadWrite,
			VisibilityAuthenticatedRead,
			VisibilityBucketOwnerRead,
			VisibilityBucketOwnerFullControl,
		}
	default:
		return nil
	}
}

func ValidateDestinationVisibility(provider string, visibility ObjectVisibility) error {
	if !visibility.IsKnown() {
		return fmt.Errorf("unsupported dest.visibility %q", visibility)
	}
	if visibility == VisibilityUnspecified || visibility == VisibilitySource {
		return nil
	}
	for _, supported := range SupportedDestinationVisibilities(provider) {
		if visibility == supported {
			return nil
		}
	}
	return fmt.Errorf("dest.visibility %q is not supported for provider %q", visibility, provider)
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

	// GetObjectVisibility returns the source object's ACL in generic form.
	GetObjectVisibility(key string) (ObjectVisibility, error)
}

// Destination is implemented by any object-storage backend that can be written to.
type Destination interface {
	// PutObjectFromStream uploads body to key.
	// size must be the exact content length; pass -1 if unknown (forces chunked).
	PutObjectFromStream(key string, body io.Reader, size int64, opts UploadOptions) error

	// Close releases any SDK-level resources.
	Close()
}

// ProbeableDestination can validate bucket-level connectivity without writing data.
type ProbeableDestination interface {
	Destination
	Probe() error
}
