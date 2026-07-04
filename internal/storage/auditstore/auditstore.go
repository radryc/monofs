package auditstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type SnapshotInfo struct {
	Key       string
	Seq       uint64
	Timestamp int64
	Size      int64
}

type Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UsePathStyle    bool
}

type Store struct {
	client *s3.Client
	bucket string
	prefix string
}

func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("auditstore: bucket is required")
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("auditstore: load AWS config: %w", err)
	}

	if cfg.AccessKeyID != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID,
			cfg.SecretAccessKey,
			cfg.SessionToken,
		)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &Store{
		client: s3Client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

func (s *Store) UploadSnapshot(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.fullKey(key)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/gzip"),
	})
	return err
}

func (s *Store) DownloadSnapshot(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("auditstore: get object %s: %w", key, err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (s *Store) ListSnapshots(ctx context.Context, prefix string) ([]SnapshotInfo, error) {
	searchPrefix := s.fullKey(prefix)

	var snapshots []SnapshotInfo
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(searchPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("auditstore: list objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			snapshots = append(snapshots, SnapshotInfo{
				Key:  key,
				Size: aws.ToInt64(obj.Size),
			})
		}
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Key < snapshots[j].Key
	})

	return snapshots, nil
}

func (s *Store) fullKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}
