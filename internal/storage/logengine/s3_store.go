package logengine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Store implements ObjectStoreBackend over AWS S3.
type S3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Store creates a new S3Store.
func NewS3Store(client *s3.Client, bucket, prefix string) *S3Store {
	return &S3Store{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}
}

// fullPath returns the S3 key with the configured prefix.
func (s *S3Store) fullPath(path string) string {
	if s.prefix == "" {
		return path
	}
	return s.prefix + "/" + strings.TrimPrefix(path, "/")
}

// Write streams data to an S3 object.
func (s *S3Store) Write(ctx context.Context, path string, reader io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullPath(path)),
		Body:   reader,
	})
	return err
}

// Read fetches an S3 object and returns it as a ReadSeekCloser.
func (s *S3Store) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	// ReadSeekCloser for S3 can be complex since streaming is easy but seeking requires HTTP Range requests.
	// For simplicity in this implementation, we will buffer the whole object in memory
	// or use a smart wrapper if available. Here we download it to a memory buffer to support ReadSeekCloser.
	// A production system might use aws-sdk-go-v2/feature/s3/manager or a custom HTTP range implementation.

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullPath(path)),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			if apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound" {
				return nil, ErrGhostChunk
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	// Download the whole object to a memory buffer to support Seeking.
	// (Warning: this might consume significant memory for large chunks).
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return &dummyReadSeekCloser{bytes.NewReader(data)}, nil
}

// ListChunks returns a list of chunk IDs inside the given prefix.
func (s *S3Store) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	searchPrefix := s.fullPath(prefix)
	if !strings.HasSuffix(searchPrefix, "/") {
		searchPrefix += "/"
	}

	// We use Delimiter "/" to get "directories" (chunks).
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(searchPrefix),
		Delimiter: aws.String("/"),
	})

	var chunks []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			// Extract the chunk ID from the prefix.
			// e.g. "prefix/chunks/chunk_1/" -> "chunk_1"
			raw := aws.ToString(cp.Prefix)
			raw = strings.TrimSuffix(raw, "/")
			parts := strings.Split(raw, "/")
			chunks = append(chunks, parts[len(parts)-1])
		}
	}
	return chunks, nil
}
