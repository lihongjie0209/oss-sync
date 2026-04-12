package store

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

// PutObjectFromStream uploads body to key.
// size >= 0 sets Content-Length; pass -1 for unknown size (chunked upload).
// Uses UNSIGNED-PAYLOAD so the body stream doesn't need to be seekable.
func (s *S3Store) PutObjectFromStream(key string, body io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
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

// Close is a no-op (AWS SDK manages connection pool internally).
func (s *S3Store) Close() {}
