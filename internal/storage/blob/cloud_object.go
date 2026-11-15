package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/radryc/monofs/internal/storage"
)

// cloudObjectInfo is the subset of remote object metadata used to decide
// whether a local archive is already backed up.
type cloudObjectInfo struct {
	Size   int64
	SHA256 string
}

// fileSHA256 returns the lower-case hex SHA-256 of a local file.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// cloudKey returns the S3/GCS object key for a local archive path.
// The key is: <prefix>/<storageID>/<filename.pack>
func (bb *BlobBackend) cloudKey(archivePath string) string {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives")
	rel, err := filepath.Rel(archiveDir, archivePath)
	if err != nil {
		// Shouldn't happen; fall back to basename.
		rel = filepath.Base(archivePath)
	}
	// Normalise to forward slashes for cloud keys.
	rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")

	prefix := bb.config.Cloud.S3Prefix
	if bb.config.StorageType == storage.StorageTypeGCS {
		prefix = bb.config.Cloud.GCSPrefix
	}
	if prefix != "" {
		return strings.TrimRight(prefix, "/") + "/" + rel
	}
	return rel
}

func (bb *BlobBackend) uploadArchiveToS3(ctx context.Context, archivePath string) error {
	key := bb.cloudKey(archivePath)
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer file.Close()

	metadata := map[string]string{}
	if fingerprint, hashErr := fileSHA256(archivePath); hashErr == nil {
		metadata["sha256"] = fingerprint
	}
	_, err = bb.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(bb.config.Cloud.S3Bucket),
		Key:      aws.String(key),
		Body:     file,
		Metadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("s3 PutObject %s: %w", key, err)
	}
	return bb.uploadArchiveIndexToS3(ctx, archivePath)
}

func (bb *BlobBackend) uploadArchiveIndexToS3(ctx context.Context, archivePath string) error {
	indexPath := archivePath + archiveIndexSuffix
	file, err := os.Open(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open archive index %s: %w", indexPath, err)
	}
	defer file.Close()
	key := bb.cloudKey(archivePath) + archiveIndexSuffix
	if _, err := bb.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
		Body:   file,
	}); err != nil {
		return fmt.Errorf("s3 PutObject %s: %w", key, err)
	}
	return nil
}

func (bb *BlobBackend) uploadArchiveToGCS(ctx context.Context, archivePath string) error {
	key := bb.cloudKey(archivePath)
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer file.Close()

	bucket := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket)
	writer := bucket.Object(key).NewWriter(ctx)
	if fingerprint, hashErr := fileSHA256(archivePath); hashErr == nil {
		writer.Metadata = map[string]string{"sha256": fingerprint}
	}
	if _, err := io.Copy(writer, file); err != nil {
		writer.Close()
		return fmt.Errorf("gcs write %s: %w", key, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("gcs close %s: %w", key, err)
	}
	return bb.uploadArchiveIndexToGCS(ctx, archivePath)
}

func (bb *BlobBackend) uploadArchiveIndexToGCS(ctx context.Context, archivePath string) error {
	indexPath := archivePath + archiveIndexSuffix
	file, err := os.Open(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open archive index %s: %w", indexPath, err)
	}
	defer file.Close()
	key := bb.cloudKey(archivePath) + archiveIndexSuffix
	bucket := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket)
	writer := bucket.Object(key).NewWriter(ctx)
	if _, err := io.Copy(writer, file); err != nil {
		writer.Close()
		return fmt.Errorf("gcs write %s: %w", key, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("gcs close %s: %w", key, err)
	}
	return nil
}

func (bb *BlobBackend) deleteCloudArchive(ctx context.Context, archivePath string) error {
	key := bb.cloudKey(archivePath)
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		// Remove the sidecar first so a concurrent read-through cannot resolve
		// a blob to an archive that is about to disappear.
		for _, objectKey := range []string{key + archiveIndexSuffix, key} {
			if _, err := bb.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bb.config.Cloud.S3Bucket),
				Key:    aws.String(objectKey),
			}); err != nil {
				return fmt.Errorf("s3 DeleteObject %s: %w", objectKey, err)
			}
		}
		return nil
	case storage.StorageTypeGCS:
		bucket := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket)
		for _, objectKey := range []string{key + archiveIndexSuffix, key} {
			if err := bucket.Object(objectKey).Delete(ctx); err != nil {
				return fmt.Errorf("gcs delete %s: %w", objectKey, err)
			}
		}
		return nil
	default:
		return nil
	}
}

func (bb *BlobBackend) openS3Reader(key string) (io.ReadCloser, error) {
	resp, err := bb.s3Client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 GetObject %s: %w", key, err)
	}
	return resp.Body, nil
}

func (bb *BlobBackend) openGCSReader(key string) (io.ReadCloser, error) {
	rc, err := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket).Object(key).NewReader(context.Background())
	if err != nil {
		return nil, fmt.Errorf("gcs NewReader %s: %w", key, err)
	}
	return rc, nil
}

func (bb *BlobBackend) headS3Object(key string) (cloudObjectInfo, bool) {
	out, err := bb.s3Client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return cloudObjectInfo{}, false
	}
	return cloudObjectInfo{Size: aws.ToInt64(out.ContentLength), SHA256: out.Metadata["sha256"]}, true
}

func (bb *BlobBackend) headGCSObject(key string) (cloudObjectInfo, bool) {
	attrs, err := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket).Object(key).Attrs(context.Background())
	if err != nil {
		return cloudObjectInfo{}, false
	}
	info := cloudObjectInfo{Size: attrs.Size}
	if attrs.Metadata != nil {
		info.SHA256 = attrs.Metadata["sha256"]
	}
	return info, true
}

// downloadFromCloud fetches an archive from the cloud and caches it locally.
func (bb *BlobBackend) downloadFromCloud(archivePath string) error {
	if !bb.hasCloudDownload() {
		return fmt.Errorf("cloud download not available for storage type %q", bb.config.StorageType)
	}

	key := bb.cloudKey(archivePath)
	reader, err := bb.openCloudReader(key)
	if err != nil {
		return err
	}
	defer reader.Close()

	// Cache locally.
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		return fmt.Errorf("create local cache dir: %w", err)
	}
	tmpPath := archivePath + ".dl.tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create local cache: %w", err)
	}
	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("stream local cache: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close local cache: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename local cache: %w", err)
	}
	if info, err := os.Stat(archivePath); err == nil {
		bb.cloudDownloadBytes.Add(info.Size())
		if bb.otel.downloadBytes != nil {
			bb.otel.downloadBytes.Add(context.Background(), info.Size())
		}
	}
	bb.cloudDownloadsTotal.Add(1)
	if bb.otel.downloads != nil {
		bb.otel.downloads.Add(context.Background(), 1)
	}
	bb.cloudRetrieves.Add(1)
	return nil
}

// cloudArchiveExists returns true if the archive has a corresponding object in
// the configured cloud storage backend. It returns false when cloud is not
// configured or when the object cannot be reached.
func (bb *BlobBackend) cloudArchiveExists(archivePath string) bool {
	if !bb.hasCloudDownload() {
		return false
	}
	if bb.headCloudObject == nil {
		return false
	}
	_, ok := bb.headCloudObject(bb.cloudKey(archivePath))
	return ok
}

// cloudArchiveMatches reports whether the cloud copy of archivePath has the
// same size and (when the remote exposes it) the same SHA-256. Existence alone
// is not enough: re-ingesting a chunk reuses the same object key.
func (bb *BlobBackend) cloudArchiveMatches(archivePath string) bool {
	if bb.headCloudObject == nil {
		return false
	}
	local, err := os.Stat(archivePath)
	if err != nil {
		return false
	}
	remote, ok := bb.headCloudObject(bb.cloudKey(archivePath))
	if !ok || remote.Size != local.Size() {
		return false
	}
	if remote.SHA256 == "" {
		// Legacy objects uploaded before metadata support: size is the best
		// available signal.
		return true
	}
	localSHA, err := fileSHA256(archivePath)
	if err != nil {
		return false
	}
	return strings.EqualFold(remote.SHA256, localSHA)
}

// ensureArchiveInCloud uploads the local archive to cloud storage if a cloud
// object does not already exist. It returns nil when the archive is already
// backed up or after a successful upload.
func (bb *BlobBackend) ensureArchiveInCloud(ctx context.Context, archivePath string) error {
	if !bb.hasCloudUpload() {
		return fmt.Errorf("cloud upload not configured")
	}
	if bb.cloudArchiveMatches(archivePath) {
		// Repair a missing sidecar index without re-uploading the payload.
		if _, err := os.Stat(archivePath + archiveIndexSuffix); err == nil && !bb.cloudArchiveIndexExists(archivePath) {
			return bb.uploadArchiveIndex(ctx, archivePath)
		}
		return nil
	}
	return bb.uploadArchiveFunc(ctx, archivePath)
}

func (bb *BlobBackend) uploadArchiveIndex(ctx context.Context, archivePath string) error {
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		return bb.uploadArchiveIndexToS3(ctx, archivePath)
	case storage.StorageTypeGCS:
		return bb.uploadArchiveIndexToGCS(ctx, archivePath)
	default:
		return nil
	}
}

func (bb *BlobBackend) cloudArchiveIndexExists(archivePath string) bool {
	if bb.headCloudObject == nil {
		return false
	}
	_, ok := bb.headCloudObject(bb.cloudKey(archivePath) + archiveIndexSuffix)
	return ok
}
