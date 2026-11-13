package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/radryc/monofs/internal/storage"
)

// EncryptionKeyStatus describes the state of the encryption-key guard.
type EncryptionKeyStatus struct {
	Pending              bool   `json:"pending"`
	State                string `json:"state"`
	KeySource            string `json:"key_source"`
	CurrentFingerprint   string `json:"current_fingerprint"`
	AcceptedFingerprint  string `json:"accepted_fingerprint"`
	StorageLocation      string `json:"storage_location"`
	FirstKeyAutoAccepted bool   `json:"first_key_auto_accepted"`
	CanConfirm           bool   `json:"can_confirm"`
	Error                string `json:"error"`
}

const acceptedKeyFingerprintPath = "_meta/accepted-key-fingerprint"

// acceptedKeyFingerprintFile returns the local filesystem path used to store
// the accepted key fingerprint when cloud storage is not configured.
func (bb *BlobBackend) acceptedKeyFingerprintFile() string {
	return filepath.Join(bb.config.CacheDir, ".accepted-key-fingerprint")
}

// verifyEncryptionKey compares the configured key against the durably stored
// accepted fingerprint. If no fingerprint has been stored yet, the current key
// is accepted automatically (first-key behaviour). If the fingerprint differs,
// the backend enters a pending state and refuses to serve blobs until an
// operator explicitly confirms the new key.
func (bb *BlobBackend) verifyEncryptionKey() error {
	current := fmt.Sprintf("%x", sha256.Sum256(bb.config.EncryptionKey))
	bb.currentKeyFingerprint = current

	accepted, location, err := bb.readAcceptedKeyFingerprint()
	if err != nil {
		return fmt.Errorf("read accepted key fingerprint: %w", err)
	}

	bb.keyGuardMu.Lock()
	bb.acceptedKeyFingerprint = accepted
	bb.keyGuardMu.Unlock()

	if accepted == "" {
		// First key ever seen: auto-accept and persist.
		if err := bb.writeAcceptedKeyFingerprint(current); err != nil {
			return fmt.Errorf("persist first accepted key fingerprint: %w", err)
		}
		bb.keyGuardMu.Lock()
		bb.acceptedKeyFingerprint = current
		bb.keyGuardMu.Unlock()
		if bb.logger != nil {
			bb.logger.Info("first encryption key auto-accepted",
				"fingerprint", current,
				"location", location)
		}
		packagerEncryptionKeyPending.Set(0)
		bb.keyGuardPending.Store(false)
		return nil
	}

	if strings.EqualFold(accepted, current) {
		packagerEncryptionKeyPending.Set(0)
		bb.keyGuardPending.Store(false)
		if bb.logger != nil {
			bb.logger.Info("encryption key verified against accepted fingerprint",
				"fingerprint", current,
				"location", location)
		}
		return nil
	}

	packagerEncryptionKeyPending.Set(1)
	bb.keyGuardPending.Store(true)
	if bb.logger != nil {
		bb.logger.Error("encryption key mismatch: explicit confirmation required",
			"current_fingerprint", current,
			"accepted_fingerprint", accepted,
			"location", location)
	}
	return nil
}

// readAcceptedKeyFingerprint reads the durably stored accepted fingerprint.
// It returns the fingerprint, the storage location description, and any error.
// An empty fingerprint with a nil error means no accepted key has been stored yet.
func (bb *BlobBackend) readAcceptedKeyFingerprint() (string, string, error) {
	if bb.hasCloudDownload() {
		location := fmt.Sprintf("%s://%s/%s", bb.config.StorageType, bb.bucketName(), acceptedKeyFingerprintPath)
		rc, err := bb.openCloudReader(acceptedKeyFingerprintPath)
		if err != nil {
			// A missing cloud object is expected on first startup.
			if bb.isMissingCloudObject(err) {
				return "", location, nil
			}
			return "", location, fmt.Errorf("read cloud accepted key fingerprint: %w", err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return "", location, fmt.Errorf("read cloud accepted key fingerprint body: %w", err)
		}
		return strings.TrimSpace(string(data)), location, nil
	}

	// Local-only storage.
	path := bb.acceptedKeyFingerprintFile()
	location := "local://" + path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", location, nil
		}
		return "", location, fmt.Errorf("read local accepted key fingerprint: %w", err)
	}
	return strings.TrimSpace(string(data)), location, nil
}

// writeAcceptedKeyFingerprint durably stores the accepted key fingerprint.
func (bb *BlobBackend) writeAcceptedKeyFingerprint(fingerprint string) error {
	if bb.hasCloudUpload() {
		return bb.writeCloudAcceptedKeyFingerprint(fingerprint)
	}
	path := bb.acceptedKeyFingerprintFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create accepted key fingerprint dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(fingerprint), 0600); err != nil {
		return fmt.Errorf("write local accepted key fingerprint: %w", err)
	}
	return nil
}

func (bb *BlobBackend) writeCloudAcceptedKeyFingerprint(fingerprint string) error {
	data := []byte(fingerprint)
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		_, err := bb.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bb.config.Cloud.S3Bucket),
			Key:    aws.String(acceptedKeyFingerprintPath),
			Body:   bytes.NewReader(data),
		})
		if err != nil {
			return fmt.Errorf("s3 PutObject %s: %w", acceptedKeyFingerprintPath, err)
		}
		return nil
	case storage.StorageTypeGCS:
		bucket := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket)
		writer := bucket.Object(acceptedKeyFingerprintPath).NewWriter(context.Background())
		if _, err := writer.Write(data); err != nil {
			writer.Close()
			return fmt.Errorf("gcs write %s: %w", acceptedKeyFingerprintPath, err)
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("gcs close %s: %w", acceptedKeyFingerprintPath, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported cloud storage type %q", bb.config.StorageType)
	}
}

// isMissingCloudObject returns true when an error from openCloudReader indicates
// that the object simply does not exist.
func (bb *BlobBackend) isMissingCloudObject(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	// S3 and GCS return well-known codes/messages for missing objects.
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "nosuchkey") ||
		strings.Contains(lower, "statuscode=404") ||
		strings.Contains(lower, "storage: object doesn't exist")
}

// bucketName returns the configured bucket name for logging/status purposes.
func (bb *BlobBackend) bucketName() string {
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		return bb.config.Cloud.S3Bucket
	case storage.StorageTypeGCS:
		return bb.config.Cloud.GCSBucket
	default:
		return ""
	}
}

// ConfirmEncryptionKey explicitly accepts the currently configured encryption
// key as the new authoritative key. After confirmation the backend will serve
// blobs again, but archives encrypted with the previous key will be unreadable.
func (bb *BlobBackend) ConfirmEncryptionKey() (EncryptionKeyStatus, error) {
	current := fmt.Sprintf("%x", sha256.Sum256(bb.config.EncryptionKey))
	if err := bb.writeAcceptedKeyFingerprint(current); err != nil {
		return bb.EncryptionKeyStatus(), fmt.Errorf("persist confirmed key fingerprint: %w", err)
	}
	bb.keyGuardMu.Lock()
	bb.acceptedKeyFingerprint = current
	bb.keyGuardMu.Unlock()
	bb.keyGuardPending.Store(false)
	packagerEncryptionKeyPending.Set(0)
	if bb.logger != nil {
		bb.logger.Info("encryption key explicitly confirmed",
			"new_fingerprint", current)
	}
	return bb.EncryptionKeyStatus(), nil
}

// EncryptionKeyStatus returns the current key-guard state.
func (bb *BlobBackend) EncryptionKeyStatus() EncryptionKeyStatus {
	bb.keyGuardMu.RLock()
	accepted := bb.acceptedKeyFingerprint
	bb.keyGuardMu.RUnlock()

	location := "local://" + bb.acceptedKeyFingerprintFile()
	if bb.isCloudConfigured() {
		location = fmt.Sprintf("%s://%s/%s", bb.config.StorageType, bb.bucketName(), acceptedKeyFingerprintPath)
	}

	pending := bb.keyGuardPending.Load()
	state := "ok"
	firstAuto := false
	if pending {
		state = "pending"
	} else if accepted == "" {
		state = "no_key"
	} else if accepted == bb.currentKeyFingerprint {
		firstAuto = true
		state = "auto_accepted"
	}

	return EncryptionKeyStatus{
		Pending:              pending,
		State:                state,
		KeySource:            bb.config.EncryptionKeySource,
		CurrentFingerprint:   bb.currentKeyFingerprint,
		AcceptedFingerprint:  accepted,
		StorageLocation:      location,
		FirstKeyAutoAccepted: firstAuto,
		CanConfirm:           pending,
	}
}

// keyGuardCheck returns an error when the encryption key has not been
// explicitly accepted. Callers should use this before mutating or serving
// archive data.
func (bb *BlobBackend) keyGuardCheck() error {
	if bb.keyGuardPending.Load() {
		return fmt.Errorf("encryption key %s is pending confirmation; existing archives may be unreadable", bb.currentKeyFingerprint)
	}
	return nil
}
