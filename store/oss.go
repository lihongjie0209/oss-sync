package store

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// OSSStore wraps the Aliyun OSS SDK and implements both Source and Destination.
type OSSStore struct {
	bucket *oss.Bucket
}

// NewOSSStore creates an authenticated OSS store.
// Set insecureSkipVerify=true for endpoints with self-signed or private-CA certificates.
func NewOSSStore(endpoint, accessKeyID, accessKeySecret, bucketName string, insecureSkipVerify bool) (*OSSStore, error) {
	var opts []oss.ClientOption
	if insecureSkipVerify {
		opts = append(opts, oss.HTTPClient(&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		}))
	}

	client, err := oss.New(endpoint, accessKeyID, accessKeySecret, opts...)
	if err != nil {
		return nil, fmt.Errorf("create oss client: %w", err)
	}

	bucket, err := client.Bucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("get oss bucket %s: %w", bucketName, err)
	}

	return &OSSStore{bucket: bucket}, nil
}

// ListPage lists a single page of objects under prefix starting at marker.
// pageToken is the OSS Marker value.
func (s *OSSStore) ListPage(prefix, pageToken string, pageSize int) ([]Object, string, bool, error) {
	result, err := s.bucket.ListObjects(
		oss.Prefix(prefix),
		oss.Marker(pageToken),
		oss.MaxKeys(pageSize),
	)
	if err != nil {
		return nil, "", false, fmt.Errorf("list oss objects: %w", err)
	}

	objects := make([]Object, 0, len(result.Objects))
	for _, obj := range result.Objects {
		objects = append(objects, Object{
			Key:          obj.Key,
			ETag:         obj.ETag,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}

	return objects, result.NextMarker, result.IsTruncated, nil
}

// GetObjectStream returns a streaming reader for key. Caller must close it.
func (s *OSSStore) GetObjectStream(key string) (io.ReadCloser, error) {
	body, err := s.bucket.GetObject(key)
	if err != nil {
		return nil, fmt.Errorf("get oss object %s: %w", key, err)
	}
	return body, nil
}

// PutObjectFromStream uploads body to key (OSSStore can also act as Destination).
func (s *OSSStore) PutObjectFromStream(key string, body io.Reader, size int64) error {
	var opts []oss.Option
	if size >= 0 {
		opts = append(opts, oss.ContentLength(size))
	}
	if err := s.bucket.PutObject(key, body, opts...); err != nil {
		return fmt.Errorf("put oss object %s: %w", key, err)
	}
	return nil
}

// Probe validates the configured OSS bucket by issuing a lightweight list request.
func (s *OSSStore) Probe() error {
	if _, _, _, err := s.ListPage("", "", 1); err != nil {
		return fmt.Errorf("probe oss bucket: %w", err)
	}
	return nil
}

// Close is a no-op for OSS (SDK manages connection pool internally).
func (s *OSSStore) Close() {}
