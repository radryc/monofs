package blob

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/radryc/monofs/internal/storage"
	"google.golang.org/api/iterator"

	gcStorage "cloud.google.com/go/storage"
)

// ListStorageObjects enumerates archives stored in the configured backend.
// For cloud storage it lists objects from the S3/GCS bucket (respecting the
// configured prefix). For local storage it enumerates .pack files under the
// local archive directory.
func (bb *BlobBackend) ListStorageObjects(ctx context.Context) ([]storage.StorageObject, error) {
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		return bb.listS3Objects(ctx)
	case storage.StorageTypeGCS:
		return bb.listGCSObjects(ctx)
	case storage.StorageTypeLocal, "":
		return bb.listLocalObjects()
	default:
		return nil, fmt.Errorf("unsupported storage type %q for object listing", bb.config.StorageType)
	}
}

func (bb *BlobBackend) listS3Objects(ctx context.Context) ([]storage.StorageObject, error) {
	if bb.s3Client == nil {
		return nil, fmt.Errorf("s3 client not initialized")
	}

	prefix := strings.TrimRight(bb.config.Cloud.S3Prefix, "/")
	if prefix != "" {
		prefix = prefix + "/"
	}

	var objects []storage.StorageObject
	var continuationToken *string
	const maxPerPage = 1000
	const maxTotal = 10000

	for {
		if len(objects) >= maxTotal {
			break
		}
		out, err := bb.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bb.config.Cloud.S3Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
			MaxKeys:           aws.Int32(maxPerPage),
		})
		if err != nil {
			return objects, fmt.Errorf("list s3 objects: %w", err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			if key == "" {
				continue
			}
			var mtime int64
			if obj.LastModified != nil {
				mtime = obj.LastModified.Unix()
			}
			objects = append(objects, storage.StorageObject{
				Key:          key,
				Size:         aws.ToInt64(obj.Size),
				LastModified: mtime,
				StorageType:  string(storage.StorageTypeS3),
				Bucket:       bb.config.Cloud.S3Bucket,
			})
			if len(objects) >= maxTotal {
				break
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}

	return objects, nil
}

// listS3ObjectsPrefix lists a bounded set of objects under one prefix. The
// configured S3Prefix is prepended, matching cloudKey().
func (bb *BlobBackend) listS3ObjectsPrefix(ctx context.Context, prefix string, maxTotal int) ([]storage.StorageObject, error) {
	if bb.s3Client == nil {
		return nil, fmt.Errorf("s3 client not initialized")
	}
	if maxTotal <= 0 {
		maxTotal = 5000
	}
	basePrefix := strings.TrimRight(bb.config.Cloud.S3Prefix, "/")
	if basePrefix != "" {
		basePrefix += "/"
	}
	fullPrefix := basePrefix + strings.TrimLeft(prefix, "/")

	var objects []storage.StorageObject
	var continuationToken *string
	const maxPerPage = 1000
	for {
		if len(objects) >= maxTotal {
			break
		}
		out, err := bb.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bb.config.Cloud.S3Bucket),
			Prefix:            aws.String(fullPrefix),
			ContinuationToken: continuationToken,
			MaxKeys:           aws.Int32(maxPerPage),
		})
		if err != nil {
			return objects, fmt.Errorf("list s3 objects under %s: %w", fullPrefix, err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			if key == "" {
				continue
			}
			var mtime int64
			if obj.LastModified != nil {
				mtime = obj.LastModified.Unix()
			}
			objects = append(objects, storage.StorageObject{
				Key:          key,
				Size:         aws.ToInt64(obj.Size),
				LastModified: mtime,
				StorageType:  string(storage.StorageTypeS3),
				Bucket:       bb.config.Cloud.S3Bucket,
			})
			if len(objects) >= maxTotal {
				break
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}
	return objects, nil
}

func (bb *BlobBackend) listGCSObjects(ctx context.Context) ([]storage.StorageObject, error) {
	if bb.gcsClient == nil {
		return nil, fmt.Errorf("gcs client not initialized")
	}

	prefix := strings.TrimRight(bb.config.Cloud.GCSPrefix, "/")
	if prefix != "" {
		prefix = prefix + "/"
	}

	var objects []storage.StorageObject
	const maxTotal = 10000

	it := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket).Objects(ctx, &gcStorage.Query{
		Prefix: prefix,
	})
	for {
		if len(objects) >= maxTotal {
			break
		}
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return objects, fmt.Errorf("list gcs objects: %w", err)
		}
		objects = append(objects, storage.StorageObject{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated.Unix(),
			StorageType:  string(storage.StorageTypeGCS),
			Bucket:       bb.config.Cloud.GCSBucket,
		})
	}

	return objects, nil
}

func (bb *BlobBackend) listLocalObjects() ([]storage.StorageObject, error) {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives")
	var objects []storage.StorageObject

	err := filepath.WalkDir(archiveDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(archiveDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
		objects = append(objects, storage.StorageObject{
			Key:          rel,
			Size:         info.Size(),
			LastModified: info.ModTime().Unix(),
			StorageType:  string(storage.StorageTypeLocal),
		})
		return nil
	})
	if err != nil {
		return objects, fmt.Errorf("list local archives: %w", err)
	}

	return objects, nil
}

func isHexString(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
