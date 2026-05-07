package store

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Store is a generic S3-compatible store (works with MinIO, AWS S3, R2, etc.).
// It implements both Source and Destination.
type S3Store struct {
	client     *s3.Client
	bucketName string
}

// NewS3Store creates an S3-compatible store.
// Set forcePathStyle=true for MinIO or any endpoint that requires path-style URLs.
func NewS3Store(endpoint, region, accessKeyID, accessKeySecret, bucketName string, forcePathStyle bool) (*S3Store, error) {
	creds := credentials.NewStaticCredentialsProvider(accessKeyID, accessKeySecret, "")

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       region,
		Credentials:  creds,
		UsePathStyle: forcePathStyle,
		// Only calculate checksums when required, not on every request.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})

	return &S3Store{client: client, bucketName: bucketName}, nil
}

// ListPage lists one page of objects under prefix.
// pageToken is the S3 ContinuationToken (empty string for first page).
func (s *S3Store) ListPage(prefix, pageToken string, pageSize int) ([]Object, string, bool, error) {
	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucketName),
		MaxKeys: aws.Int32(int32(pageSize)),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}
	if pageToken != "" {
		input.ContinuationToken = aws.String(pageToken)
	}

	result, err := s.client.ListObjectsV2(context.Background(), input)
	if err != nil {
		return nil, "", false, fmt.Errorf("list s3 objects: %w", err)
	}

	objects := make([]Object, 0, len(result.Contents))
	for _, obj := range result.Contents {
		etag := ""
		if obj.ETag != nil {
			etag = *obj.ETag
		}
		lastMod := obj.LastModified
		size := int64(0)
		if obj.Size != nil {
			size = *obj.Size
		}
		objects = append(objects, Object{
			Key:          aws.ToString(obj.Key),
			ETag:         etag,
			Size:         size,
			LastModified: aws.ToTime(lastMod),
		})
	}

	nextToken := ""
	if result.NextContinuationToken != nil {
		nextToken = *result.NextContinuationToken
	}

	return objects, nextToken, aws.ToBool(result.IsTruncated), nil
}

// GetObjectStream returns a streaming reader for key. Caller must close it.
func (s *S3Store) GetObjectStream(key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get s3 object %s: %w", key, err)
	}
	return result.Body, nil
}

// GetObjectVisibility returns the source object's canned ACL when it can be inferred.
func (s *S3Store) GetObjectVisibility(key string) (ObjectVisibility, error) {
	result, err := s.client.GetObjectAcl(context.Background(), &s3.GetObjectAclInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return VisibilityUnspecified, fmt.Errorf("get s3 object acl %s: %w", key, err)
	}
	visibility, err := inferS3Visibility(result)
	if err != nil {
		return VisibilityUnspecified, fmt.Errorf("infer s3 object acl %s: %w", key, err)
	}
	return visibility, nil
}

// PutObjectFromStream uploads body to key.
// size >= 0 sets Content-Length; pass -1 for unknown size (chunked upload).
// Uses UNSIGNED-PAYLOAD so the body stream doesn't need to be seekable.
func (s *S3Store) PutObjectFromStream(key string, body io.Reader, size int64, opts UploadOptions) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	if acl, ok := toS3ACL(opts.Visibility); ok {
		input.ACL = acl
	}

	// v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware replaces the
	// payload-hash computation with "UNSIGNED-PAYLOAD", allowing non-seekable
	// streaming bodies without buffering the entire object in memory.
	_, err := s.client.PutObject(context.Background(), input,
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware),
	)
	if err != nil {
		return fmt.Errorf("put s3 object %s: %w", key, err)
	}
	return nil
}

// Probe validates the configured bucket without creating any object.
func (s *S3Store) Probe() error {
	_, err := s.client.HeadBucket(context.Background(), &s3.HeadBucketInput{
		Bucket: aws.String(s.bucketName),
	})
	if err != nil {
		return fmt.Errorf("head s3 bucket %s: %w", s.bucketName, err)
	}
	return nil
}

// Close is a no-op (AWS SDK manages connection pool internally).
func (s *S3Store) Close() {}

func toS3ACL(visibility ObjectVisibility) (types.ObjectCannedACL, bool) {
	switch visibility {
	case VisibilityPrivate:
		return types.ObjectCannedACLPrivate, true
	case VisibilityPublicRead:
		return types.ObjectCannedACLPublicRead, true
	case VisibilityPublicReadWrite:
		return types.ObjectCannedACLPublicReadWrite, true
	case VisibilityAuthenticatedRead:
		return types.ObjectCannedACLAuthenticatedRead, true
	case VisibilityBucketOwnerRead:
		return types.ObjectCannedACLBucketOwnerRead, true
	case VisibilityBucketOwnerFullControl:
		return types.ObjectCannedACLBucketOwnerFullControl, true
	default:
		return "", false
	}
}

func inferS3Visibility(result *s3.GetObjectAclOutput) (ObjectVisibility, error) {
	if result == nil {
		return VisibilityUnspecified, fmt.Errorf("empty acl result")
	}

	ownerID := ""
	if result.Owner != nil && result.Owner.ID != nil {
		ownerID = aws.ToString(result.Owner.ID)
	}

	groupPerms := map[string]map[types.Permission]bool{}
	canonicalPerms := map[string]map[types.Permission]bool{}

	for _, grant := range result.Grants {
		if grant.Grantee == nil {
			continue
		}
		grantee := grant.Grantee
		switch grantee.Type {
		case types.TypeGroup:
			uri := aws.ToString(grantee.URI)
			if uri == "" {
				continue
			}
			if groupPerms[uri] == nil {
				groupPerms[uri] = map[types.Permission]bool{}
			}
			groupPerms[uri][grant.Permission] = true
		case types.TypeCanonicalUser:
			id := aws.ToString(grantee.ID)
			if id == "" || id == ownerID {
				continue
			}
			if canonicalPerms[id] == nil {
				canonicalPerms[id] = map[types.Permission]bool{}
			}
			canonicalPerms[id][grant.Permission] = true
		}
	}

	const (
		allUsersURI           = "http://acs.amazonaws.com/groups/global/AllUsers"
		authenticatedUsersURI = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
	)

	if perms := groupPerms[allUsersURI]; len(perms) > 0 {
		if perms[types.PermissionRead] && (perms[types.PermissionWriteAcp] || perms[types.PermissionFullControl]) {
			return VisibilityPublicReadWrite, nil
		}
		if perms[types.PermissionRead] {
			return VisibilityPublicRead, nil
		}
		return VisibilityUnspecified, fmt.Errorf("unsupported AllUsers grant set")
	}

	if perms := groupPerms[authenticatedUsersURI]; len(perms) > 0 {
		if perms[types.PermissionRead] {
			return VisibilityAuthenticatedRead, nil
		}
		return VisibilityUnspecified, fmt.Errorf("unsupported AuthenticatedUsers grant set")
	}

	if len(canonicalPerms) == 1 {
		for _, perms := range canonicalPerms {
			if hasOnlyPermission(perms, types.PermissionRead) {
				return VisibilityBucketOwnerRead, nil
			}
			if hasOnlyPermission(perms, types.PermissionFullControl) {
				return VisibilityBucketOwnerFullControl, nil
			}
		}
	}

	if len(groupPerms) == 0 && len(canonicalPerms) == 0 {
		return VisibilityPrivate, nil
	}

	return VisibilityUnspecified, fmt.Errorf("unsupported custom ACL grants")
}

func hasOnlyPermission(perms map[types.Permission]bool, permission types.Permission) bool {
	return len(perms) == 1 && perms[permission]
}
