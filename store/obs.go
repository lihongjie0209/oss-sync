package store

import (
	"fmt"
	"io"

	obs "github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
)

// OBSStore wraps the Huawei OBS SDK and implements Destination.
type OBSStore struct {
	client     *obs.ObsClient
	bucketName string
}

// NewOBSStore creates an authenticated OBS store.
func NewOBSStore(endpoint, accessKeyID, accessKeySecret, bucketName string) (*OBSStore, error) {
	client, err := obs.New(accessKeyID, accessKeySecret, endpoint)
	if err != nil {
		return nil, fmt.Errorf("create obs client: %w", err)
	}
	return &OBSStore{client: client, bucketName: bucketName}, nil
}

// PutObjectFromStream uploads body to key in OBS.
// size must be the exact content length; pass -1 if unknown (chunked transfer).
func (s *OBSStore) PutObjectFromStream(key string, body io.Reader, size int64, opts UploadOptions) error {
	input := &obs.PutObjectInput{}
	input.Bucket = s.bucketName
	input.Key = key
	input.Body = body

	if size >= 0 {
		input.ContentLength = size
	}
	if acl, ok := toOBSAcl(opts.Visibility); ok {
		input.ACL = acl
	}

	if _, err := s.client.PutObject(input); err != nil {
		return fmt.Errorf("put obs object %s: %w", key, err)
	}
	return nil
}

// Probe validates the configured OBS bucket without writing test data.
func (s *OBSStore) Probe() error {
	if _, err := s.client.HeadBucket(s.bucketName); err != nil {
		return fmt.Errorf("head obs bucket %s: %w", s.bucketName, err)
	}
	return nil
}

func toOBSAcl(visibility ObjectVisibility) (obs.AclType, bool) {
	switch visibility {
	case VisibilityPrivate:
		return obs.AclPrivate, true
	case VisibilityPublicRead:
		return obs.AclPublicRead, true
	case VisibilityPublicReadWrite:
		return obs.AclPublicReadWrite, true
	case VisibilityAuthenticatedRead:
		return obs.AclAuthenticatedRead, true
	case VisibilityBucketOwnerRead:
		return obs.AclBucketOwnerRead, true
	case VisibilityBucketOwnerFullControl:
		return obs.AclBucketOwnerFullControl, true
	default:
		return "", false
	}
}

// Close releases OBS client resources.
func (s *OBSStore) Close() {
	s.client.Close()
}
