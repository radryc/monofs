# Internal Storage Documentation

## Table of Contents

- [storage/interface.go](#storageinterfacego)
- [storage/stats.go](#storagestatsgo)
- [storage/auditstore/auditstore.go](#storageauditstoreauditstorego)
- [storage/blob/backend.go](#storageblobbackendgo)
- [storage/blob/metrics.go](#storageblobmetricsgo)
- [storage/file/ingestion.go](#storagefileingestiongo)
- [storage/git/fetch.go](#storagegitfetchgo)
- [storage/git/fetcher_backend.go](#storagegitfetcher_backendgo)
- [storage/git/ingestion.go](#storagegitingestiongo)
- [storage/logengine/engine.go](#storagelogengineenginego)
- [storage/logengine/ingester.go](#storagelogengineingestergo)
- [storage/logengine/metrics.go](#storagelogenginemetricsgo)
- [storage/logengine/models.go](#storagelogenginemodelsgo)
- [storage/logengine/query.go](#storagelogenginequerygo)
- [storage/logengine/s3_store.go](#storagelogengines3_storego)
- [storage/logengine/storage.go](#storagelogenginestoragego)
- [storage/logquery/parser.go](#storagelogqueryparsergo)
- [storage/workspacestore/store.go](#storageworkspacestorestorego)
- [storage/workspacestore/types.go](#storageworkspacestoretypesgo)
- [storage/workspacestore/wal.go](#storageworkspacestorewalgo)

---

## storage/interface.go

Package `storage` defines the core type system and interfaces for the monofs storage layer.

### Types

**`FetchType.String() string`**

- Signature: `func (ft FetchType) String() string`
- Returns the string representation of a FetchType. Returns `"unknown"` for zero-value FetchType.
- Called by: formatting/logging throughout the project.

**`ParseFetchType(s string) FetchType`**

- Signature: `func ParseFetchType(s string) FetchType`
- Converts a string (`"git"`, `"blob"`, `"s3"`, `"local"`) to the corresponding `FetchType` constant. Returns `FetchTypeUnknown` for unrecognized values.
- Called by: configuration parsing routines that map string backends to typed enums.

**`NewBackendRegistry() *BackendRegistry`**

- Signature: `func NewBackendRegistry() *BackendRegistry`
- Allocates a new `BackendRegistry` with empty maps for ingestion, fetch, and storage backend factories.
- Called by: initialization code that sets up the global `DefaultRegistry`.

**`(*BackendRegistry).RegisterIngestionBackend(t IngestionType, factory func() IngestionBackend)`**

- Signature: `func (r *BackendRegistry) RegisterIngestionBackend(t IngestionType, factory func() IngestionBackend)`
- Registers a factory function for the given `IngestionType`. Called during init to map `"git"` / `"file"` to their constructors.
- Called by: `init()` functions in ingestion backend packages.

**`(*BackendRegistry).RegisterFetchBackend(t FetchType, factory func() FetchBackend)`**

- Signature: `func (r *BackendRegistry) RegisterFetchBackend(t FetchType, factory func() FetchBackend)`
- Registers a factory function for the given `FetchType`.
- Called by: `init()` functions in fetch backend packages.

**`(*BackendRegistry).RegisterStorageBackend(t string, factory func() StorageBackend)`**

- Signature: `func (r *BackendRegistry) RegisterStorageBackend(t string, factory func() StorageBackend)`
- Registers a factory for a `StorageBackend` identified by a string type (e.g. `"logengine"`).
- Called by: `init()` functions in storage backend packages.

**`(*BackendRegistry).CreateIngestionBackend(t IngestionType) (IngestionBackend, error)`**

- Signature: `func (r *BackendRegistry) CreateIngestionBackend(t IngestionType) (IngestionBackend, error)`
- Looks up the registered factory for the type and calls it. Returns an error if the type is not registered.
- Called by: the router when it needs to create an ingestion pipeline.

**`(*BackendRegistry).CreateFetchBackend(t FetchType) (FetchBackend, error)`**

- Signature: `func (r *BackendRegistry) CreateFetchBackend(t FetchType) (FetchBackend, error)`
- Looks up the registered factory for the type and calls it. Returns an error if unknown.
- Called by: the fetcher service and router.

**`(*BackendRegistry).CreateStorageBackend(t string) (StorageBackend, error)`**

- Signature: `func (r *BackendRegistry) CreateStorageBackend(t string) (StorageBackend, error)`
- Looks up the registered factory for the string type and calls it.
- Called by: services that need special-purpose storage backends (e.g. Doctor Partition).

**`(*BackendRegistry).ListIngestionTypes() []IngestionType`**

- Signature: `func (r *BackendRegistry) ListIngestionTypes() []IngestionType`
- Returns a slice of all registered ingestion types.
- Called by: administrative/debug endpoints.

**`(*BackendRegistry).ListFetchTypes() []FetchType`**

- Signature: `func (r *BackendRegistry) ListFetchTypes() []FetchType`
- Returns a slice of all registered fetch types.
- Called by: administrative/debug endpoints.

**`(*BackendRegistry).ListStorageBackendTypes() []string`**

- Signature: `func (r *BackendRegistry) ListStorageBackendTypes() []string`
- Returns a slice of all registered storage backend type strings.
- Called by: administrative/debug endpoints.

---

## storage/stats.go

Package `storage` — atomic, lock-free statistics counter using `atomic.Pointer` and Compare-And-Swap.

### Functions

**`NewAtomicStats() *AtomicStats`**

- Signature: `func NewAtomicStats() *AtomicStats`
- Allocates a new `AtomicStats` and stores a zero-value `BackendStats` pointer.
- Called by: `NewBlobBackend()`, `NewGitBackend()`, and any code that creates a stats tracker.

**`(*AtomicStats).RecordSuccess(duration time.Duration, bytes int64)`**

- Signature: `func (s *AtomicStats) RecordSuccess(duration time.Duration, bytes int64)`
- Atomically increments `Requests`, adds `bytes` to `BytesFetched`, and recalculates the rolling `AvgLatencyMs` using a weighted average formula. Uses a CAS loop for lock-freedom.
- Called by: `GitBackend.FetchBlob()` on a successful fetch.

**`(*AtomicStats).RecordFetchHit()`**

- Signature: `func (s *AtomicStats) RecordFetchHit()`
- Atomically increments `Requests` and `CacheHits`.
- Called by: backends when serving from local cache.

**`(*AtomicStats).RecordFetchMiss()`**

- Signature: `func (s *AtomicStats) RecordFetchMiss()`
- Atomically increments `CacheMisses`. Does not increment `Requests` (the caller is responsible for that).
- Called by: backends when the index has no entry for a requested blob.

**`(*AtomicStats).RecordError()`**

- Signature: `func (s *AtomicStats) RecordError()`
- Atomically increments `Requests` and `Errors` by one.
- Called by: `GitBackend.FetchBlob()` on errors.

**`(*AtomicStats).RecordNotFound()`**

- Signature: `func (s *AtomicStats) RecordNotFound()`
- Atomically increments `Requests`, `Errors`, and `CacheMisses` by one. Used when a blob is confirmed absent.
- Called by: fetch backends on definite not-found results.

**`(*AtomicStats).GetStats() BackendStats`**

- Signature: `func (s *AtomicStats) GetStats() BackendStats`
- Returns a copy of the current stats (dereferences the atomic pointer).
- Called by: `GitBackend.Stats()`.

**`(*AtomicStats).Store(stats *BackendStats)`**

- Signature: `func (s *AtomicStats) Store(stats *BackendStats)`
- Directly stores a new `BackendStats` pointer. Used for initialization and bulk updates (e.g. after scanning archives).
- Called by: `BlobBackend` during `scanArchives`, `StoreArchive`, `StoreBlob`, `DeleteBlobs`, etc.

**`(*AtomicStats).Load() *BackendStats`**

- Signature: `func (s *AtomicStats) Load() *BackendStats`
- Returns the current stats pointer for in-place mutation + re-store patterns.
- Called by: `BlobBackend.FetchBlob()`, `BlobBackend.Stats()`, etc.

---

## storage/auditstore/auditstore.go

Package `auditstore` manages audit snapshots stored in S3-compatible object storage.

### Types

**`SnapshotInfo`** — Metadata for a stored snapshot. Fields: `Key`, `Seq`, `Timestamp`, `Size`.
**`Config`** — S3 connection configuration. Fields: `Endpoint`, `Region`, `Bucket`, `Prefix`, `AccessKeyID`, `SecretAccessKey`, `SessionToken`, `UsePathStyle`.
**`Store`** — Holds `*s3.Client`, `bucket`, and `prefix` strings.

### Functions

**`New(ctx context.Context, cfg Config) (*Store, error)`**

- Signature: `func New(ctx context.Context, cfg Config) (*Store, error)`
- Validates that `Bucket` is non-empty, loads default AWS config, optionally applies static credentials if `AccessKeyID` is set, creates an S3 client with optional endpoint override and path-style setting.
- Returns `*Store` configured with the client, bucket, and prefix.
- Called by: application initialization code that sets up the audit store.

**`(*Store).UploadSnapshot(ctx context.Context, key string, data []byte) error`**

- Signature: `func (s *Store) UploadSnapshot(ctx context.Context, key string, data []byte) error`
- Calls `PutObject` on the S3 client with `ContentType: "application/gzip"`, using the prefixed key.
- Called by: the audit snapshot creation workflow.

**`(*Store).DownloadSnapshot(ctx context.Context, key string) ([]byte, error)`**

- Signature: `func (s *Store) DownloadSnapshot(ctx context.Context, key string) ([]byte, error)`
- Issues `GetObject`, reads the entire response body into memory, and returns the bytes. Returns a wrapped error on failure.
- Called by: audit snapshot restore/verification workflows.

**`(*Store).ListSnapshots(ctx context.Context, prefix string) ([]SnapshotInfo, error)`**

- Signature: `func (s *Store) ListSnapshots(ctx context.Context, prefix string) ([]SnapshotInfo, error)`
- Paginates through `ListObjectsV2` results with the given search prefix, collects key+size for each object, and sorts the results by key ascending.
- Called by: admin UI, audit listing endpoints.

**`(*Store).fullKey(key string) string`**

- Signature: `func (s *Store) fullKey(key string) string`
- Prepends the configured prefix to the key (with `"/"` separator) if a prefix is set; otherwise returns the key unchanged.
- Called by: `UploadSnapshot`, `DownloadSnapshot`, `ListSnapshots`.

---

## storage/blob/backend.go

Package `blob` implements the packager-archive blob fetch backend. Archives are encrypted (ChaCha20-Poly1305), compressed (zstd), and support O(1) random-access reads.

### Types

- **`cloudArchiveUploadJob`** — Holds `archivePath` for async cloud upload.
- **`countingWriter`** — Wraps an `io.Writer` and tracks bytes written.
- **`archiveRef`** — Maps a blob hash to its archive location: `archivePath` (filesystem path to `.pack` file) + `entryPath` (path within the archive).
- **`openArchive`** — Wraps a `*packager.ArchiveReader` with `ObjectReader` and `lastUsed` time for LRU eviction.
- **`BlobBackend`** — The main backend struct (detailed fields at lines 72-125).

### Functions — BlobBackend

**`NewBlobBackend() *BlobBackend`**

- Signature: `func NewBlobBackend() *BlobBackend`
- Allocates a new `BlobBackend` with empty blob index, archive cache, storage ID map, and zero stats. Initializes the health flag to `true`.
- Called by: fetcher service initialization.

**`(*BlobBackend).Type() storage.FetchType`**

- Signature: `func (bb *BlobBackend) Type() storage.FetchType`
- Returns `storage.FetchTypeBlob`.
- Called by: the fetch router to select the correct backend.

**`(*BlobBackend).Initialize(ctx context.Context, config storage.BackendConfig) error`**

- Signature: `func (bb *BlobBackend) Initialize(ctx context.Context, config storage.BackendConfig) error`
- Validates the 32-byte encryption key. Creates a packager pipeline. Initializes the cloud storage client (S3 or GCS) if configured. Creates the local archive cache directory. Scans existing archives on disk to build the blob index. Starts cloud upload workers and the cloud backup integrity scanner. Verifies the encryption key fingerprint.
- Called by: the fetcher at startup.

**`(*BlobBackend).SetLogger(logger *slog.Logger)`**

- Signature: `func (bb *BlobBackend) SetLogger(logger *slog.Logger)`
- Stores a logger reference on the backend.
- Called by: fetcher setup.

**`(*BlobBackend).startCloudUploadWorkers()`**

- Signature: `func (bb *BlobBackend) startCloudUploadWorkers()`
- If cloud upload is configured and workers are not already running, creates a buffered channel and spawns `Concurrency` goroutines running `cloudUploadWorker`.
- Called by: `Initialize`.

**`(*BlobBackend).cloudUploadWorker(workerID int)`**

- Signature: `func (bb *BlobBackend) cloudUploadWorker(workerID int)`
- Drains `cloudUploadQueue` in a loop, calling `uploadArchiveFunc` for each job. On failure, marks storage unhealthy and logs the error. On success, increments `cloudStores` and resets the health flag.
- Called by: `startCloudUploadWorkers` (spawned as goroutines).

**`(*BlobBackend).queueCloudUpload(archivePath string)`**

- Signature: `func (bb *BlobBackend) queueCloudUpload(archivePath string)`
- If cloud upload is not configured, returns immediately. If the upload queue hasn't been started yet (nil channel), performs a synchronous upload. Otherwise sends the job to the channel, blocking if the buffer is full.
- Called by: `StoreArchive`, `StoreBlob`, `sealCurrentArchive`.

**`(*BlobBackend).startCloudBackupScanner()`**

- Signature: `func (bb *BlobBackend) startCloudBackupScanner()`
- If cloud is configured and no scanner is already running, creates a stop channel and spawns `cloudBackupScannerLoop`.
- Called by: `Initialize`.

**`(*BlobBackend).cloudBackupScannerLoop()`**

- Signature: `func (bb *BlobBackend) cloudBackupScannerLoop()`
- Sleeps 30s after startup, then runs a scan. Ticks every 5 minutes thereafter. Stops when `scannerStop` is closed.
- Called by: `startCloudBackupScanner` (spawned as goroutine).

**`(*BlobBackend).runCloudBackupScan()`**

- Signature: `func (bb *BlobBackend) runCloudBackupScan()`
- Iterates over all known archive paths. For each one, calls `cloudArchiveExists` — if missing, increments `packagerCloudBackupMissingTotal` and attempts to repair by calling `ensureArchiveInCloud` with a 5-minute timeout. Records the scan duration in `packagerCloudBackupScanDurationSeconds`.
- Called by: `cloudBackupScannerLoop`.

**`(*BlobBackend).acceptedKeyFingerprintFile() string`**

- Signature: `func (bb *BlobBackend) acceptedKeyFingerprintFile() string`
- Returns the local filesystem path `<CacheDir>/.accepted-key-fingerprint`.
- Called by: `readAcceptedKeyFingerprint`, `writeAcceptedKeyFingerprint`, `EncryptionKeyStatus`.

**`(*BlobBackend).verifyEncryptionKey() error`**

- Signature: `func (bb *BlobBackend) verifyEncryptionKey() error`
- Computes the SHA-256 fingerprint of the current encryption key. Reads the durably stored accepted fingerprint. If no fingerprint has been stored yet, auto-accepts and persists the current key. If the fingerprints match, the key is verified. If they differ, sets `keyGuardPending` to true and sets the corresponding Prometheus gauge to 1.
- Called by: `Initialize`.

**`(*BlobBackend).readAcceptedKeyFingerprint() (string, string, error)`**

- Signature: `func (bb *BlobBackend) readAcceptedKeyFingerprint() (string, string, error)`
- Returns (fingerprint, storageLocation, error). If cloud download is available, reads from the cloud object at `_meta/accepted-key-fingerprint`. Otherwise reads from a local file. Returns empty fingerprint with nil error when no key has been stored yet.
- Called by: `verifyEncryptionKey`.

**`(*BlobBackend).writeAcceptedKeyFingerprint(fingerprint string) error`**

- Signature: `func (bb *BlobBackend) writeAcceptedKeyFingerprint(fingerprint string) error`
- If cloud upload is available, delegates to `writeCloudAcceptedKeyFingerprint`. Otherwise writes to a local file at `acceptedKeyFingerprintFile()` with mode `0600`.
- Called by: `verifyEncryptionKey`, `ConfirmEncryptionKey`.

**`(*BlobBackend).writeCloudAcceptedKeyFingerprint(fingerprint string) error`**

- Signature: `func (bb *BlobBackend) writeCloudAcceptedKeyFingerprint(fingerprint string) error`
- Writes the fingerprint string to cloud storage. For S3: uses `PutObject`. For GCS: creates a writer on the bucket object. Returns error for unsupported storage types.
- Called by: `writeAcceptedKeyFingerprint`.

**`(*BlobBackend).isMissingCloudObject(err error) bool`**

- Signature: `func (bb *BlobBackend) isMissingCloudObject(err error) bool`
- Returns `true` when the error message contains well-known "not found" codes/messages for S3 (`NoSuchKey`, `NotFound`, `StatusCode=404`) or GCS (`storage: object doesn't exist`).
- Called by: `readAcceptedKeyFingerprint`.

**`(*BlobBackend).bucketName() string`**

- Signature: `func (bb *BlobBackend) bucketName() string`
- Returns the S3 or GCS bucket name based on the configured `StorageType`. Returns `""` for local storage.
- Called by: `readAcceptedKeyFingerprint`, `EncryptionKeyStatus`.

**`(*BlobBackend).ConfirmEncryptionKey() (EncryptionKeyStatus, error)`**

- Signature: `func (bb *BlobBackend) ConfirmEncryptionKey() (EncryptionKeyStatus, error)`
- Persists the current key's SHA-256 fingerprint, updates in-memory state, clears the pending flag, and sets the Prometheus gauge to 0. Returns the new key status.
- Called by: operator/admin API endpoints after deliberate key rotation.

**`(*BlobBackend).EncryptionKeyStatus() EncryptionKeyStatus`**

- Signature: `func (bb *BlobBackend) EncryptionKeyStatus() EncryptionKeyStatus`
- Returns the current key-guard state: whether the key is pending confirmation, the storage location, current/accepted fingerprints, and whether first-key auto-accept happened.
- Called by: `ConfirmEncryptionKey`, admin status endpoints.

**`(*BlobBackend).keyGuardCheck() error`**

- Signature: `func (bb *BlobBackend) keyGuardCheck() error`
- Returns an error if `keyGuardPending` is true, meaning the encryption key has not been explicitly accepted and blobs should not be served/modified.
- Called by: `StoreArchive`, `StoreBlob`, `StoreBlobBatch`, `DeleteBlobs`, `FetchBlob`.

**`(*BlobBackend).scanArchives(archiveDir string) error`**

- Signature: `func (bb *BlobBackend) scanArchives(archiveDir string) error`
- Reads all subdirectories of the archive directory (each is a `storageID`). For each, globs `*.pack` files, calls `indexArchive` to populate the blob index, and accumulates total file/byte counts. Handles decryption errors gracefully by logging and continuing.
- Called by: `Initialize`.

**`(*BlobBackend).indexArchive(archivePath string) (int, error)`**

- Signature: `func (bb *BlobBackend) indexArchive(archivePath string) (int, error)`
- Opens a `.pack` file as a local file reader, decrypts via packager pipeline, and lists all file entries. Indexes entries that are hex strings (possibly prefixed with `sha256:`). Stores each as an `archiveRef` in `blobIndex`. Returns the count of indexed files.
- Called by: `scanArchives`, `StoreArchive`.

**`(*BlobBackend).cloudKey(archivePath string) string`**

- Signature: `func (bb *BlobBackend) cloudKey(archivePath string) string`
- Computes the S3/GCS object key from a local archive path. Normalizes path separators to `/`, prepends the configured cloud prefix.
- Called by: `uploadArchiveToS3`, `uploadArchiveToGCS`, `downloadFromCloud`, `cloudArchiveExists`.

**`(*BlobBackend).uploadArchiveToS3(ctx context.Context, archivePath string) error`**

- Signature: `func (bb *BlobBackend) uploadArchiveToS3(ctx context.Context, archivePath string) error`
- Opens the local archive file and calls `PutObject` to upload it to the configured S3 bucket.
- Called by: set as `uploadArchiveFunc` in `Initialize` (S3 path).

**`(*BlobBackend).uploadArchiveToGCS(ctx context.Context, archivePath string) error`**

- Signature: `func (bb *BlobBackend) uploadArchiveToGCS(ctx context.Context, archivePath string) error`
- Opens the local archive file, creates a GCS writer on the bucket object, copies the data, closes the writer.
- Called by: set as `uploadArchiveFunc` in `Initialize` (GCS path).

**`(*BlobBackend).openS3Reader(key string) (io.ReadCloser, error)`**

- Signature: `func (bb *BlobBackend) openS3Reader(key string) (io.ReadCloser, error)`
- Issues `GetObject` to S3 and returns the response body reader.
- Called by: set as `openCloudReader` in `Initialize` (S3 path).

**`(*BlobBackend).openGCSReader(key string) (io.ReadCloser, error)`**

- Signature: `func (bb *BlobBackend) openGCSReader(key string) (io.ReadCloser, error)`
- Creates a `NewReader` on the GCS bucket object and returns it.
- Called by: set as `openCloudReader` in `Initialize` (GCS path).

**`(*BlobBackend).downloadFromCloud(archivePath string) error`**

- Signature: `func (bb *BlobBackend) downloadFromCloud(archivePath string) error`
- Downloads an archive from cloud storage using `openCloudReader`, writes it to a temp file, and atomically renames to the target local path. Increments `cloudRetrieves`.
- Called by: `getArchiveReader` on local cache miss.

**`(*BlobBackend).hasCloudUpload() bool`**

- Signature: `func (bb *BlobBackend) hasCloudUpload() bool`
- Returns `true` if `uploadArchiveFunc` is non-nil.
- Called by: `startCloudUploadWorkers`, `queueCloudUpload`, `isCloudConfigured`, `ensureArchiveInCloud`.

**`(*BlobBackend).hasCloudDownload() bool`**

- Signature: `func (bb *BlobBackend) hasCloudDownload() bool`
- Returns `true` if `openCloudReader` is non-nil.
- Called by: `readAcceptedKeyFingerprint`, `downloadFromCloud`, `cloudArchiveExists`, `isCloudConfigured`.

**`(*BlobBackend).cloudArchiveExists(archivePath string) bool`**

- Signature: `func (bb *BlobBackend) cloudArchiveExists(archivePath string) bool`
- Tries to open a cloud reader for the archive's cloud key. Returns `true` if successful (reader opened + closed); `false` otherwise.
- Called by: `runCloudBackupScan`, `ensureArchiveInCloud`, `DeleteBlobs`.

**`(*BlobBackend).ensureArchiveInCloud(ctx context.Context, archivePath string) error`**

- Signature: `func (bb *BlobBackend) ensureArchiveInCloud(ctx context.Context, archivePath string) error`
- If a cloud object already exists for the archive, returns nil. Otherwise uploads using `uploadArchiveFunc`.
- Called by: `runCloudBackupScan`, `DeleteBlobs`.

**`(*BlobBackend).isCloudConfigured() bool`**

- Signature: `func (bb *BlobBackend) isCloudConfigured() bool`
- Returns `true` if either cloud upload or cloud download is available.
- Called by: `startCloudBackupScanner`, `EncryptionKeyStatus`, `DeleteBlobs`.

**`(*BlobBackend).StoreArchive(storageID string, chunkIndex int, data io.Reader) (int64, int, error)`**

- Signature: `func (bb *BlobBackend) StoreArchive(storageID string, chunkIndex int, data io.Reader) (int64, int, error)`
- Checks the key guard, creates the archive directory, writes the reader data to a temp file, atomically renames it to `chunk-%04d.pack`, queues a cloud upload, evicts any cached reader, indexes the archive, updates stats. Returns `(bytesWritten, filesIndexed, error)`.
- Called by: the router during ingestion to push pre-built archives to the fetcher.

**`(*BlobBackend).StoreBlob(blobHash string, content []byte) error`**

- Signature: `func (bb *BlobBackend) StoreBlob(blobHash string, content []byte) error`
- Checks the key guard, performs a double-checked lookup in the blob index (using `singleflight` for concurrent dedup), and if the blob doesn't exist, creates a single-file archive under `_loose/`, writes the blob encrypted into it, queues cloud upload, updates the index and stats. Idempotent: if the blob already exists (or was created concurrently), returns nil.
- Called by: ingestion workflows writing individual blobs.

**`StoreBlobBatchWriter.startNewArchive() error`**

- Signature: `func (w *StoreBlobBatchWriter) startNewArchive() error`
- Creates a new `.pack` file under `_batch/` with an epoch-nano + sequence number filename. Initializes a `packager.ArchiveWriter` with the backend's pipeline.
- Called by: `NewStoreBlobBatchWriter`, `AddBlob` (when archive exceeds max size).

**`(*StoreBlobBatchWriter).AddBlob(blobHash string, content []byte)`**

- Signature: `func (w *StoreBlobBatchWriter) AddBlob(blobHash string, content []byte)`
- Skips empty hashes/content and blobs already in the index. If the current archive would exceed 512 MB, seals the current archive and starts a new one. Writes the blob into the current archive and appends the hash to `curHashes`.
- Called by: `StoreBlobBatch` and stream-based batch ingestion.

**`(*StoreBlobBatchWriter).sealCurrentArchive() error`**

- Signature: `func (w *StoreBlobBatchWriter) sealCurrentArchive() error`
- Closes the archive writer and temp file, renames to the final archive path, queues a cloud upload, updates the blob index and stats for all accumulated hashes, increments archive count.
- Called by: `AddBlob` (on size threshold), `Finish`.

**`(*StoreBlobBatchWriter).Finish() (*StoreBlobBatchResult, error)`**

- Signature: `func (w *StoreBlobBatchWriter) Finish() (*StoreBlobBatchResult, error)`
- Seals the current archive (if any) and returns the aggregate result.
- Called by: `StoreBlobBatch`.

**`(*BlobBackend).NewStoreBlobBatchWriter() (*StoreBlobBatchWriter, error)`**

- Signature: `func (bb *BlobBackend) NewStoreBlobBatchWriter() (*StoreBlobBatchWriter, error)`
- Creates the `_batch` archive directory and returns a new `StoreBlobBatchWriter` with a fresh archive open.
- Called by: `StoreBlobBatch`.

**`(*BlobBackend).StoreBlobBatch(blobs map[string][]byte) (*StoreBlobBatchResult, error)`**

- Signature: `func (bb *BlobBackend) StoreBlobBatch(blobs map[string][]byte) (*StoreBlobBatchResult, error)`
- Checks the key guard, creates a batch writer, sorts blob hashes, adds each blob, and seals.
- Called by: ingestion workflows when multiple blobs need to be stored together.

**`(*BlobBackend).DeleteBlobs(hashes []string, compact bool) *DeleteBlobsResult`**

- Signature: `func (bb *BlobBackend) DeleteBlobs(hashes []string, compact bool) *DeleteBlobsResult`
- If the key is pending, fails all deletions. Removes each blob from the in-memory index. If `compact` is true, identifies archives that become empty, ensures they have a verified cloud backup before deleting local files (blocking deletion if cloud backup is missing), and removes empty archive readers from the cache.
- Called by: garbage collection, space reclamation workflows.

**`(*BlobBackend).getArchiveReader(archivePath string) (*packager.ArchiveReader, error)`**

- Signature: `func (bb *BlobBackend) getArchiveReader(archivePath string) (*packager.ArchiveReader, error)`
- Returns a cached `ArchiveReader` with LRU tracking, updating `lastUsed`. If not cached, opens the local file. On local cache miss with cloud configured, downloads from cloud first. Enforces `maxOpenArchives` (256) via LRU eviction.
- Called by: `FetchBlob`.

**`(*BlobBackend).evictOldestLocked()`**

- Signature: `func (bb *BlobBackend) evictOldestLocked()`
- Finds and closes the least recently used open archive. Must be called with `archiveMu` held.
- Called by: `getArchiveReader`.

**`(*BlobBackend).evictArchiveReader(archivePath string)`**

- Signature: `func (bb *BlobBackend) evictArchiveReader(archivePath string)`
- If the specified archive is cached, closes its reader and store and removes it from the cache.
- Called by: `StoreArchive`.

**`(*BlobBackend).FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`**

- Signature: `func (bb *BlobBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`
- Checks the key guard. Looks up `req.ContentID` in the blob index. On miss, attempts `findBlobOnDisk` to recover. Opens the archive reader, extracts the file via `GetFile`, updates stats and Prometheus metrics. Returns the decrypted content in a `FetchResult`.
- Called by: the fetcher service for runtime blob reads.

**`(*BlobBackend).findBlobOnDisk(blobHash string) (archiveRef, bool, error)`**

- Signature: `func (bb *BlobBackend) findBlobOnDisk(blobHash string) (archiveRef, bool, error)`
- Walks all `.pack` files under the archive directory, opens each archive, and checks if the blob hash is present. Used as a recovery path when the in-memory index doesn't contain a blob. Returns the `archiveRef`, a bool indicating whether found, and any error.
- Called by: `FetchBlob` on index miss.

**`(*BlobBackend).FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`**

- Signature: `func (bb *BlobBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`
- Delegates to `FetchBlob` and wraps the content bytes in `io.NopCloser`.
- Called by: streaming read paths.

**`(*BlobBackend).Warmup(ctx context.Context, sourceKey string, config map[string]string) error`**

- Signature: `func (bb *BlobBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error`
- No-op for blob backend (archives are pushed during ingestion).
- Called by: the fetcher to satisfy the `FetchBackend` interface.

**`(*BlobBackend).CachedSources() []string`**

- Signature: `func (bb *BlobBackend) CachedSources() []string`
- Returns the sorted list of storage IDs that have archives on this fetcher.
- Called by: administrative endpoints.

**`(*BlobBackend).Cleanup(ctx context.Context, sourceKey string) error`**

- Signature: `func (bb *BlobBackend) Cleanup(ctx context.Context, sourceKey string) error`
- Removes all archive readers cached for a given `sourceKey`, deletes all blob index entries pointing to its archive directory, removes the storage ID, and deletes all local files under that archive directory.
- Called by: workspace cleanup/teardown.

**`(*BlobBackend).Close() error`**

- Signature: `func (bb *BlobBackend) Close() error`
- Stops the cloud backup scanner and waits for its goroutine. Closes the cloud upload queue and waits for workers. Closes all open archive readers and clears the cache. Closes the GCS client if any. S3 client has no Close method.
- Called by: graceful shutdown.

**`(*BlobBackend).Stats() storage.BackendStats`**

- Signature: `func (bb *BlobBackend) Stats() storage.BackendStats`
- Returns a `BackendStats` snapshot. Overwrites `CachedItems` with archive (pack) count. Includes `CloudStores` and `CloudRetrieves` counters.
- Called by: the fetcher service, admin endpoints.

**`(*BlobBackend).StorageHealth() (bool, string)`**

- Signature: `func (bb *BlobBackend) StorageHealth() (bool, string)`
- Returns `(isHealthy, lastErrorString)`. Tracks the last cloud storage error.
- Called by: health check endpoints.

**`(*BlobBackend).StorageStats() map[string]int64`**

- Signature: `func (bb *BlobBackend) StorageStats() map[string]int64`
- Returns a copy of the file count per storage ID. Special keys: `"_batch"` and `"_loose"`.
- Called by: admin/debug endpoints.

**`(*BlobBackend).HasBlob(blobHash string) bool`**

- Signature: `func (bb *BlobBackend) HasBlob(blobHash string) bool`
- Checks if a blob exists in the in-memory index.
- Called by: fetcher deduplication checks.

**`(*BlobBackend).ArchiveCount() int`**

- Signature: `func (bb *BlobBackend) ArchiveCount() int`
- Returns the number of distinct storage IDs with archives.
- Called by: admin endpoints.

**`(*BlobBackend).ListStorageObjects(ctx context.Context) ([]storage.StorageObject, error)`**

- Signature: `func (bb *BlobBackend) ListStorageObjects(ctx context.Context) ([]storage.StorageObject, error)`
- Dispatches to `listS3Objects`, `listGCSObjects`, or `listLocalObjects` based on storage type. Satisfies the `storage.ObjectLister` interface.
- Called by: storage browsing/admin endpoints.

**`(*BlobBackend).listS3Objects(ctx context.Context) ([]storage.StorageObject, error)`**

- Signature: `func (bb *BlobBackend) listS3Objects(ctx context.Context) ([]storage.StorageObject, error)`
- Lists S3 objects with pagination (max 1000 per page, max 10000 total), respecting the configured prefix. Returns `StorageObject` entries with key, size, mtime, bucket, and storage type.
- Called by: `ListStorageObjects`.

**`(*BlobBackend).listGCSObjects(ctx context.Context) ([]storage.StorageObject, error)`**

- Signature: `func (bb *BlobBackend) listGCSObjects(ctx context.Context) ([]storage.StorageObject, error)`
- Iterates GCS bucket objects with the configured prefix, up to 10000 max. Returns `StorageObject` entries.
- Called by: `ListStorageObjects`.

**`(*BlobBackend).listLocalObjects() ([]storage.StorageObject, error)`**

- Signature: `func (bb *BlobBackend) listLocalObjects() ([]storage.StorageObject, error)`
- Walks the local archive directory, collecting relative paths with normalized slashes as keys, along with size, mtime, and storage type.
- Called by: `ListStorageObjects`.

### Functions — Package-Level

**`(*countingWriter).Write(p []byte) (int, error)`**

- Signature: `func (w *countingWriter) Write(p []byte) (int, error)`
- Delegates to the inner writer and accumulates bytes written in `w.written`.
- Called by: `ArchiveWriter` during batch blob writes to track archive size.

**`isHexString(s string) bool`**

- Signature: `func isHexString(s string) bool`
- Returns `true` if the string is non-empty and contains only lowercase hex characters `[0-9a-f]`.
- Called by: `indexArchive` to filter blob hash entries from archive file listings.

---

## storage/blob/metrics.go

Package `blob` — Prometheus metric declarations for the packager archive backend.

### Package-Level Variables (Unexported, auto-registered via `promauto`)

All are `monofs_packager_*` metrics:

- **`packagerFetchBlobTotal`** — CounterVec labelled by `storage_type`. Counts blob fetch operations.
- **`packagerFetchBytesTotal`** — CounterVec labelled by `storage_type`. Counts bytes read.
- **`packagerFetchErrorsTotal`** — CounterVec labelled by `storage_type`. Counts fetch errors.
- **`packagerStoreArchiveBytesTotal`** — Counter. Bytes written as `.pack` archives.
- **`packagerStoreArchivesTotal`** — Counter. Archive chunks stored.
- **`packagerStoreBlobsTotal`** — CounterVec labelled by `store_type` (`single`, `batch`). Individual blobs stored.
- **`packagerIndexedBlobsGauge`** — Gauge. Current number of blobs indexed in memory.
- **`packagerCloudBackupMissingTotal`** — Counter. Archives missing from cloud during integrity scans.
- **`packagerCloudBackupRepairedTotal`** — Counter. Archives re-uploaded by the integrity scanner.
- **`packagerCloudBackupScanDurationSeconds`** — Histogram. Scan duration with buckets `[1,5,15,30,60,120,300]`.
- **`packagerLocalArchiveDeletionBlockedTotal`** — Counter. Local deletions blocked due to missing cloud backup.
- **`packagerEncryptionKeyPending`** — Gauge. `1` when encryption key is pending confirmation, `0` otherwise.

No exported or unexported functions in this file — only package-level variable declarations.

---

## storage/file/ingestion.go

Package `file` implements `storage.IngestionBackend` for local filesystem directories (with optional Git repo detection).

### Types

**`FileIngestionBackend`** — Fields: `repoMgr`, `repo`, `branch`, `repoID`, `sourceDir`, `isGitRepo`, `plainDirHash`.

### Functions

**`NewFileIngestionBackend() storage.IngestionBackend`**

- Signature: `func NewFileIngestionBackend() storage.IngestionBackend`
- Returns a new `*FileIngestionBackend`. Satisfies `storage.IngestionBackend` interface.
- Called by: registration in `DefaultRegistry`.

**`(*FileIngestionBackend).Type() storage.IngestionType`**

- Signature: `func (f *FileIngestionBackend) Type() storage.IngestionType`
- Returns `storage.IngestionTypeFile` (`"file"`).

**`(*FileIngestionBackend).Validate(ctx context.Context, sourceURL string, config map[string]string) error`**

- Signature: `func (f *FileIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error`
- Stats the sourceURL path. Returns an error if the path doesn't exist or is not a directory.
- Called by: the router before ingestion.

**`(*FileIngestionBackend).Initialize(ctx context.Context, sourceURL string, config map[string]string) error`**

- Signature: `func (f *FileIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error`
- Sets `sourceDir`, resolves `repoID` from config keys (`repo_id` > `display_path` > basename of sourceURL). If `.git` directory exists, calls `initializeGit`; otherwise calls `initializePlain`.
- Called by: the router to prepare for walking files.

**`(*FileIngestionBackend).initializeGit(ctx context.Context, config map[string]string) error`**

- Signature: `func (f *FileIngestionBackend) initializeGit(ctx context.Context, config map[string]string) error`
- Opens the local Git repo via `git.PlainOpen`, gets HEAD ref and branch name, creates a `RepoManager` with a temp directory, reads the current commit's hash/time/message into `config`.
- Called by: `Initialize` when a `.git` directory is present.

**`(*FileIngestionBackend).initializePlain(config map[string]string) error`**

- Signature: `func (f *FileIngestionBackend) initializePlain(config map[string]string) error`
- Sets branch to `"default"`, computes a deterministic `plainDirHash` from the source directory path (SHA-256, first 8 hex chars, prefixed with `"local-"`), and injects commit metadata into `config`.
- Called by: `Initialize` for non-Git directories.

**`(*FileIngestionBackend).WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error`**

- Signature: `func (f *FileIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error`
- Dispatches to `walkGit` or `walkPlain` based on `isGitRepo`.
- Called by: the ingestion pipeline.

**`(*FileIngestionBackend).walkGit(ctx context.Context, fn func(storage.FileMetadata) error) error`**

- Signature: `func (f *FileIngestionBackend) walkGit(ctx context.Context, fn func(storage.FileMetadata) error) error`
- Gets HEAD commit info, then uses `repoMgr.WalkTree` to iterate all files in the repo at the current branch. Reads each blob's content and constructs `FileMetadata` with Git-specific metadata (branch, repo_url, commit_hash/time/message).
- Called by: `WalkFiles`.

**`(*FileIngestionBackend).walkPlain(ctx context.Context, fn func(storage.FileMetadata) error) error`**

- Signature: `func (f *FileIngestionBackend) walkPlain(ctx context.Context, fn func(storage.FileMetadata) error) error`
- Uses `filepath.WalkDir` to iterate every file under `sourceDir`. Skips `.git` directories and symlinks. Reads each file's content, computes a SHA-256 content hash, computes the relative path, and passes `FileMetadata` to the callback.
- Called by: `WalkFiles`.

**`(*FileIngestionBackend).GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error)`**

- Signature: `func (f *FileIngestionBackend) GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error)`
- Dispatches to `getMetadataGit` or `getMetadataPlain`.
- Called by: the router for individual file metadata lookups.

**`(*FileIngestionBackend).getMetadataGit(ctx context.Context, filePath string) (*storage.FileMetadata, error)`**

- Signature: `func (f *FileIngestionBackend) getMetadataGit(ctx context.Context, filePath string) (*storage.FileMetadata, error)`
- Uses `repoMgr.GetFileMetadata` to get Git-level metadata for a single file. Returns a `FileMetadata` with path, size, mode, mtime, blob hash.
- Called by: `GetMetadata`.

**`(*FileIngestionBackend).getMetadataPlain(filePath string) (*storage.FileMetadata, error)`**

- Signature: `func (f *FileIngestionBackend) getMetadataPlain(filePath string) (*storage.FileMetadata, error)`
- Joins `sourceDir` and `filePath`, stats the file, returns `FileMetadata` with OS-level info.
- Called by: `GetMetadata`.

**`(*FileIngestionBackend).Cleanup() error`**

- Signature: `func (f *FileIngestionBackend) Cleanup() error`
- If a `repoMgr` was created, calls `CleanupRepo(repoID)` to delete the cloned copy.
- Called by: ingestion pipeline teardown.

---

## storage/git/fetch.go

Package `git` — `GitFetchBackend`, a lightweight fetch backend that reads blobs via the internal git package's `RepoManager` and `BlobCache`.

### Types

**`GitFetchBackend`** — Fields: `repoMgr *gitpkg.RepoManager`, `blobCache *gitpkg.BlobCache`, `cacheDir`.

### Functions

**`NewGitFetchBackend() storage.FetchBackend`**

- Signature: `func NewGitFetchBackend() storage.FetchBackend`
- Returns a new `*GitFetchBackend`.
- Called by: registration in `DefaultRegistry`.

**`(*GitFetchBackend).Type() storage.FetchType`**

- Signature: `func (g *GitFetchBackend) Type() storage.FetchType`
- Returns `storage.FetchTypeGit`.

**`(*GitFetchBackend).Initialize(ctx context.Context, config storage.BackendConfig) error`**

- Signature: `func (g *GitFetchBackend) Initialize(ctx context.Context, config storage.BackendConfig) error`
- Resolves `cacheDir` from `config.CacheDir` or `config.Extra["cache_dir"]`. Creates a `RepoManager` and a `BlobCache` with default config (overridable via `config.Extra["blob_cache_dir"]`).
- Called by: the fetcher service at startup.

**`(*GitFetchBackend).FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`**

- Signature: `func (g *GitFetchBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`
- Delegates to `blobCache.ReadBlob(ctx, nil, req.ContentID)`. Returns the blob data wrapped in a `FetchResult` with `FromCache: true`.
- Called by: the fetcher when a Git-type blob is requested.

**`(*GitFetchBackend).FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`**

- Signature: `func (g *GitFetchBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`
- Calls `FetchBlob` and wraps the result bytes in an `io.NopCloser`.
- Called by: streaming fetch paths.

**`(*GitFetchBackend).Warmup(ctx context.Context, sourceKey string, config map[string]string) error`**

- Signature: `func (g *GitFetchBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error`
- No-op. Returns nil.
- Called by: `FetchBackend` interface compliance.

**`(*GitFetchBackend).CachedSources() []string`**

- Signature: `func (g *GitFetchBackend) CachedSources() []string`
- Returns nil.
- Called by: `FetchBackend` interface compliance.

**`(*GitFetchBackend).Cleanup(ctx context.Context, sourceKey string) error`**

- Signature: `func (g *GitFetchBackend) Cleanup(ctx context.Context, sourceKey string) error`
- No-op. Returns nil.
- Called by: `FetchBackend` interface compliance.

**`(*GitFetchBackend).Close() error`**

- Signature: `func (g *GitFetchBackend) Close() error`
- Closes the `blobCache` if non-nil.
- Called by: graceful shutdown.

**`(*GitFetchBackend).Stats() storage.BackendStats`**

- Signature: `func (g *GitFetchBackend) Stats() storage.BackendStats`
- Returns empty `BackendStats`.
- Called by: `FetchBackend` interface compliance.

---

## storage/git/fetcher_backend.go

Package `git` — `GitBackend`, a heavier fetch backend that directly manages `go-git` repository clones with cache cleanup.

### Types

**`GitBackend`** — Fields: `config`, `repos map[string]*cachedRepo` (protected by `sync.RWMutex`), `stats *storage.AtomicStats`.
**`cachedRepo`** — Fields: `repo *gogit.Repository`, `repoPath`, `lastAccess time.Time`, `mu sync.Mutex`.

### Functions

**`NewGitBackend() *GitBackend`**

- Signature: `func NewGitBackend() *GitBackend`
- Allocates a new `GitBackend` with empty repo map and fresh `AtomicStats`.
- Called by: configuration-driven backend selection.

**`(*GitBackend).Type() storage.FetchType`**

- Signature: `func (gb *GitBackend) Type() storage.FetchType`
- Returns `storage.FetchTypeGit`.

**`(*GitBackend).Initialize(ctx context.Context, cfg storage.BackendConfig) error`**

- Signature: `func (gb *GitBackend) Initialize(ctx context.Context, cfg storage.BackendConfig) error`
- Saves config, creates cache directory, scans existing cloned repos (reading their `origin` remote URL to rebuild the `repos` map), and starts a background `cleanupLoop` goroutine.
- Called by: fetcher setup.

**`(*GitBackend).FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`**

- Signature: `func (gb *GitBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error)`
- Extracts `repo_url` and `branch` from `req.SourceConfig` (defaults branch to `"main"`). Gets or clones the repo via `getOrCloneRepo`. Reads the blob by hash via `repo.BlobObject(hash)`. On failure, attempts `fetchLatest` first. Reads all blob content into memory and records success stats.
- Called by: the fetcher for runtime git blob requests.

**`(*GitBackend).FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`**

- Signature: `func (gb *GitBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error)`
- Like `FetchBlob` but returns `blob.Reader()` directly (and `blob.Size`) instead of reading all content. Returns the open reader — caller must close.
- Called by: streaming fetch paths.

**`(*GitBackend).Warmup(ctx context.Context, sourceKey string, config map[string]string) error`**

- Signature: `func (gb *GitBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error`
- Calls `getOrCloneRepo` with the source key as repo URL.
- Called by: the fetcher to pre-populate cache.

**`(*GitBackend).CachedSources() []string`**

- Signature: `func (gb *GitBackend) CachedSources() []string`
- Returns slice of repo URLs currently cached.
- Called by: admin endpoints.

**`(*GitBackend).Cleanup(ctx context.Context, sourceKey string) error`**

- Signature: `func (gb *GitBackend) Cleanup(ctx context.Context, sourceKey string) error`
- Removes the repo from the map and deletes its on-disk files via `os.RemoveAll`.
- Called by: workspace teardown.

**`(*GitBackend).Close() error`**

- Signature: `func (gb *GitBackend) Close() error`
- Clears the repos map (does not delete disk files — they can be reused on restart).
- Called by: graceful shutdown.

**`(*GitBackend).Stats() storage.BackendStats`**

- Signature: `func (gb *GitBackend) Stats() storage.BackendStats`
- Returns the current `AtomicStats` snapshot.
- Called by: admin endpoints.

**`(*GitBackend).getOrCloneRepo(ctx context.Context, repoURL, branch string) (*cachedRepo, error)`**

- Signature: `func (gb *GitBackend) getOrCloneRepo(ctx context.Context, repoURL, branch string) (*cachedRepo, error)`
- Double-checked locking pattern: tries read lock first, then write lock. Attempts `PlainOpen` on the cached path first; if missing, does a shallow (depth=1), single-branch, no-checkout clone. Caches the opened repo.
- Called by: `FetchBlob`, `FetchBlobStream`, `Warmup`.

**`(*GitBackend).fetchLatest(ctx context.Context, cached *cachedRepo, branch string) error`**

- Signature: `func (gb *GitBackend) fetchLatest(ctx context.Context, cached *cachedRepo, branch string) error`
- Executes a depth-1 fetch for the given branch. Returns nil if already up-to-date.
- Called by: `FetchBlob`, `FetchBlobStream` when a blob is not found in the cached repo.

**`(*GitBackend).cleanupLoop(ctx context.Context)`**

- Signature: `func (gb *GitBackend) cleanupLoop(ctx context.Context)`
- Ticks every 5 minutes. Calls `cleanupOldRepos` with the configured max age (default 1 hour if unset).
- Called by: `Initialize` (spawned as goroutine).

**`(*GitBackend).cleanupOldRepos(maxAge time.Duration)`**

- Signature: `func (gb *GitBackend) cleanupOldRepos(maxAge time.Duration)`
- Iterates cached repos and removes any whose `lastAccess` exceeds `maxAge`. Deletes on-disk files.
- Called by: `cleanupLoop`.

**`hashString(s string) string`**

- Signature: `func hashString(s string) string`
- Simple hash function for generating directory names from strings. Uses multiplicative hash (31), returns 16-char hex string.
- Called by: `getOrCloneRepo` to compute the cache subdirectory name.

---

## storage/git/ingestion.go

Package `git` — `GitIngestionBackend`, implements `storage.IngestionBackend` for Git repositories.

### Types

**`GitIngestionBackend`** — Fields: `repoMgr *gitpkg.RepoManager`, `repo *git.Repository`, `branch`, `repoID`, `sourceURL`.

### Functions

**`NewGitIngestionBackend() storage.IngestionBackend`**

- Signature: `func NewGitIngestionBackend() storage.IngestionBackend`
- Returns a new `*GitIngestionBackend`.
- Called by: registration in `DefaultRegistry`.

**`(*GitIngestionBackend).Type() storage.IngestionType`**

- Signature: `func (g *GitIngestionBackend) Type() storage.IngestionType`
- Returns `storage.IngestionTypeGit` (`"git"`).

**`(*GitIngestionBackend).Initialize(ctx context.Context, sourceURL string, config map[string]string) error`**

- Signature: `func (g *GitIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error`
- Sets `sourceURL` and `branch` (default `"main"`). Resolves `repoID` from `config["display_path"]` or normalizes from URL. Creates a `RepoManager` in a temp directory. Clones or opens the repo. Extracts commit hash/time/message into `config`.
- Called by: the router before walking.

**`(*GitIngestionBackend).Validate(ctx context.Context, sourceURL string, config map[string]string) error`**

- Signature: `func (g *GitIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error`
- Creates a temporary `RepoManager`, calls `GetDefaultBranch` on it, and cleans up. Returns error if the repo has no branches.
- Called by: the router before ingestion.

**`(*GitIngestionBackend).WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error`**

- Signature: `func (g *GitIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error`
- Gets HEAD commit info, then walks the repo tree via `repoMgr.WalkTree`. For each file, reads the blob content and constructs `FileMetadata` with commit metadata.
- Called by: the ingestion pipeline.

**`(*GitIngestionBackend).GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error)`**

- Signature: `func (g *GitIngestionBackend) GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error)`
- Uses `repoMgr.GetFileMetadata` for the specific file path and branch. Returns a `FileMetadata` with basic Git-level info.
- Called by: the router.

**`(*GitIngestionBackend).Cleanup() error`**

- Signature: `func (g *GitIngestionBackend) Cleanup() error`
- Calls `repoMgr.CleanupRepo(repoID)`.
- Called by: ingestion pipeline teardown.

**`normalizeRepoID(repoURL string) string`**

- Signature: `func normalizeRepoID(repoURL string) string`
- Extracts the repository name from a URL using `filepath.Base` (simple implementation). For example, `"https://github.com/foo/bar.git"` → `"bar.git"`.
- Called by: `Initialize` when `display_path` is not set.

---

## storage/logengine/engine.go

Package `logengine` — LogEngine is the main interface for the high-compression, searchable log, metric & trace engine.

### Types

**`LogEngine`** — Fields: `store *CachedStore`, `ingester *Ingester`, `query *QueryEngine`.
**`Config`** — Fields: `LocalCacheDir string`, `ChunkDuration time.Duration`.
**`MockS3Store`** — Local filesystem mock implementing `ObjectStoreBackend` for testing.
**`dummyReadSeekCloser`** — Wraps `*bytes.Reader` for testing.
**`tempFileReadSeekCloser`** — Wraps `*os.File` and deletes the file on Close.
**`LogEngineStats`** — Fields: `LogChunks`, `MetricChunks`, `TraceChunks` (all `int64`).

### Functions — LogEngine

**`New(backend ObjectStoreBackend, cfg Config) *LogEngine`**

- Signature: `func New(backend ObjectStoreBackend, cfg Config) *LogEngine`
- Creates a `CachedStore` around the backend, then creates `Ingester` and `QueryEngine` referencing that store.
- Called by: application initialization.

**`(*LogEngine).Type() string`**

- Signature: `func (e *LogEngine) Type() string`
- Returns `"logengine"`.

**`(*LogEngine).Initialize(ctx context.Context, config storage.BackendConfig) error`**

- Signature: `func (e *LogEngine) Initialize(ctx context.Context, config storage.BackendConfig) error`
- Stub: returns nil. Not fully implemented.
- Called by: storage backend interface compliance.

**`(*LogEngine).IngestLogs(ctx context.Context, chunkID string, logs []LogRecord) error`**

- Signature: `func (e *LogEngine) IngestLogs(ctx context.Context, chunkID string, logs []LogRecord) error`
- Delegates to `ingester.FlushChunk(ctx, SignalLogs, chunkID, logs, nil, nil)`.
- Called by: log ingestion pipelines.

**`(*LogEngine).IngestMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error`**

- Signature: `func (e *LogEngine) IngestMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error`
- Delegates to `ingester.FlushChunk(ctx, SignalMetrics, chunkID, nil, metrics, nil)`.
- Called by: metric ingestion pipelines.

**`(*LogEngine).IngestTraces(ctx context.Context, chunkID string, spans []SpanRecord) error`**

- Signature: `func (e *LogEngine) IngestTraces(ctx context.Context, chunkID string, spans []SpanRecord) error`
- Delegates to `ingester.FlushChunk(ctx, SignalTraces, chunkID, nil, nil, spans)`.
- Called by: trace span ingestion pipelines.

**`(*LogEngine).Ingest(ctx context.Context, id string, data []byte) error`**

- Signature: `func (e *LogEngine) Ingest(ctx context.Context, id string, data []byte) error`
- No-op passthrough for the `storage.StorageBackend` interface.
- Called by: generic storage backend interface.

**`(*LogEngine).QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error)`**

- Signature: `func (e *LogEngine) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error)`
- Delegates to `query.QueryLogs`.
- Called by: API handlers for log queries.

**`(*LogEngine).StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error`**

- Signature: `func (e *LogEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error`
- Delegates to `query.StreamLogs`.
- Called by: streaming log query paths.

**`(*LogEngine).QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`**

- Signature: `func (e *LogEngine) QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`
- Delegates to `query.QueryMetrics`.
- Called by: API handlers for metric queries.

**`(*LogEngine).StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error`**

- Signature: `func (e *LogEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error`
- Delegates to `query.StreamMetrics`.
- Called by: streaming metric query paths.

**`(*LogEngine).QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error)`**

- Signature: `func (e *LogEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error)`
- Delegates to `query.QueryTraces`.
- Called by: API handlers for trace queries.

**`(*LogEngine).StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error`**

- Signature: `func (e *LogEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error`
- Delegates to `query.StreamTraces`.
- Called by: streaming trace query paths.

**`(*LogEngine).Query(ctx context.Context, queryStr string) ([]byte, error)`**

- Signature: `func (e *LogEngine) Query(ctx context.Context, queryStr string) ([]byte, error)`
- Calls `query.QueryLogs` with no service/time filters and no limit, then JSON-marshals the result. Implements the `storage.StorageBackend` interface.
- Called by: gRPC compat layer.

**`(*LogEngine).Close() error`**

- Signature: `func (e *LogEngine) Close() error`
- Returns nil. No-op.
- Called by: graceful shutdown.

**`(*LogEngine).Stats(ctx context.Context) (LogEngineStats, error)`**

- Signature: `func (e *LogEngine) Stats(ctx context.Context) (LogEngineStats, error)`
- Lists chunk directories for logs, metrics, and traces signals, counting the entries.
- Called by: admin status endpoints.

### Functions — MockS3Store

**`NewMockS3Store(baseDir string) *MockS3Store`**

- Signature: `func NewMockS3Store(baseDir string) *MockS3Store`
- Creates a new mock with the given base directory.
- Called by: tests.

**`(*MockS3Store).Write(ctx context.Context, path string, reader io.Reader) error`**

- Signature: `func (s *MockS3Store) Write(ctx context.Context, path string, reader io.Reader) error`
- Creates/overwrites a file under `baseDir/path` by copying from the reader.
- Called by: `Ingester` (via the `ObjectStoreBackend` interface in tests).

**`(*MockS3Store).Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`**

- Signature: `func (s *MockS3Store) Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`
- Opens the file. Returns `ErrGhostChunk` if the file does not exist.
- Called by: `CachedStore` and `QueryEngine` in tests.

**`(*MockS3Store).ListChunks(ctx context.Context, prefix string) ([]string, error)`**

- Signature: `func (s *MockS3Store) ListChunks(ctx context.Context, prefix string) ([]string, error)`
- Reads subdirectory names under `baseDir/prefix`.
- Called by: `CachedStore.ListChunks` in tests.

### Functions — Utility Types

**`(*dummyReadSeekCloser).Close() error`** — Returns nil.
**`(*tempFileReadSeekCloser).Close() error`** — Closes the underlying file and removes it from disk.

---

## storage/logengine/ingester.go

Package `logengine` — `Ingester` handles the chunking and dual-write architecture (Parquet + Bluge full-text index + metadata).

### Types

**`Ingester`** — Fields: `store ObjectStoreBackend`, `chunkDur time.Duration`.

### Functions — Ingester

**`NewIngester(store ObjectStoreBackend, chunkDur time.Duration) *Ingester`**

- Signature: `func NewIngester(store ObjectStoreBackend, chunkDur time.Duration) *Ingester`
- Creates a new ingester with the given backend and chunk duration.
- Called by: `New` in `engine.go`.

**`(*Ingester).FlushChunk(ctx context.Context, signal Signal, chunkID string, logs []LogRecord, metrics []MetricRecord, spans []SpanRecord) error`**

- Signature: `func (i *Ingester) FlushChunk(ctx context.Context, signal Signal, chunkID string, logs []LogRecord, metrics []MetricRecord, spans []SpanRecord) error`
- Dispatches based on `signal` to `flushLogs`, `flushMetrics`, or `flushTraces`. Exactly one of logs/metrics/spans should be non-nil.
- Called by: `LogEngine.IngestLogs`, `IngestMetrics`, `IngestTraces`.

**`(*Ingester).flushLogs(ctx context.Context, chunkID string, logs []LogRecord) error`**

- Signature: `func (i *Ingester) flushLogs(ctx context.Context, chunkID string, logs []LogRecord) error`
- Computes `minTime`/`maxTime`, collects service names, builds a `ChunkManifest`. Concurrently writes: (1) Parquet data file, (2) Bluge full-text search index (tar.gz), (3) metadata JSON. Returns first error.
- Called by: `FlushChunk`.

**`(*Ingester).flushMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error`**

- Signature: `func (i *Ingester) flushMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error`
- Computes time range, collects services/metric names/label values, builds manifest. Concurrently writes Parquet + metadata.
- Called by: `FlushChunk`.

**`(*Ingester).flushTraces(ctx context.Context, chunkID string, spans []SpanRecord) error`**

- Signature: `func (i *Ingester) flushTraces(ctx context.Context, chunkID string, spans []SpanRecord) error`
- Computes time range (including end times), collects service names, builds a 256-byte Bloom filter over trace IDs. Concurrently writes Parquet + metadata.
- Called by: `FlushChunk`.

**`(*Ingester).writeLogParquet(ctx context.Context, path string, logs []LogRecord) error`**

- Signature: `func (i *Ingester) writeLogParquet(ctx context.Context, path string, logs []LogRecord) error`
- Builds a Parquet schema with columns: `timestamp` (int64 UnixNano), `level` (ByteArray), `service` (ByteArray), `trace_id` (ByteArray), `raw_message` (ByteArray). Uses Zstd compression with dictionary encoding. Writes all log records in a single row group and uploads via `store.Write`.
- Called by: `flushLogs`.

**`(*Ingester).writeMetricParquet(ctx context.Context, path string, metrics []MetricRecord) error`**

- Signature: `func (i *Ingester) writeMetricParquet(ctx context.Context, path string, metrics []MetricRecord) error`
- Parquet schema: `timestamp` (int64), `service` (ByteArray), `metric_name` (ByteArray), `value` (float64), `labels_json` (ByteArray, JSON-serialized map). Zstd + dictionary compression.
- Called by: `flushMetrics`.

**`(*Ingester).writeTraceParquet(ctx context.Context, path string, spans []SpanRecord) error`**

- Signature: `func (i *Ingester) writeTraceParquet(ctx context.Context, path string, spans []SpanRecord) error`
- Parquet schema: `timestamp`, `end_time`, `trace_id`, `span_id`, `parent_span_id`, `service`, `name`, `status_code`, `attributes_json`. Uses helper functions `writeByteArrayCol` and `writeInt64Col` for cleaner code.
- Called by: `flushTraces`.

**`(*Ingester).writeBlugeIndex(ctx context.Context, path string, logs []LogRecord) error`**

- Signature: `func (i *Ingester) writeBlugeIndex(ctx context.Context, path string, logs []LogRecord) error`
- Creates a temporary Bluge index directory, adds one document per log record indexing the `raw_message` field with position 0 (row ID as document ID), closes the writer, then tars+gzips the entire index directory and uploads via `store.Write`. Cleans up temp directory afterwards.
- Called by: `flushLogs`.

**`(*Ingester).writeMetadata(ctx context.Context, path string, manifest ChunkManifest) error`**

- Signature: `func (i *Ingester) writeMetadata(ctx context.Context, path string, manifest ChunkManifest) error`
- JSON-encodes the manifest and uploads via `store.Write`.
- Called by: `flushLogs`, `flushMetrics`, `flushTraces`.

### Package-Level Helper Functions

**`collectLogServices(logs []LogRecord) []string`** — Extracts unique non-empty service names from a log record batch. Returns sorted strings.
**`collectMetricServices(metrics []MetricRecord) []string`** — Same for metrics.
**`collectMetricNames(metrics []MetricRecord) []string`** — Extracts unique metric names.
**`collectMetricLabelValues(metrics []MetricRecord) map[string][]string`** — Groups unique label values by label name.
**`collectTraceServices(spans []SpanRecord) []string`** — Extracts unique non-empty service names from spans. Returns sorted strings.
**`sortedKeys(values map[string]struct{}) []string`** — Converts a set to a sorted string slice. Returns nil for empty maps.
**`buildTraceBloom(spans []SpanRecord) []byte`** — Builds a 256-byte Bloom filter using FNV-based double hashing with 4 hash functions. Bits in the bloom filter indicate trace IDs present in the span batch.
**`bloomIndexes(value string, bitCount, hashCount int) []int`** — Computes bit positions for a value in a Bloom filter using FNV-1a + FNV secondary hashing, returning `hashCount` indexes.

---

## storage/logengine/metrics.go

Package `logengine` — Prometheus metrics and observation helper for telemetry query monitoring.

### Package-Level Variables (Unexported, auto-registered via `promauto`)

All prefixed `monofs_logengine_*`:

- **`logengineQueriesTotal`** — CounterVec labelled by `signal`.
- **`logengineQueryDurationSeconds`** — HistogramVec labelled by `signal` (end-to-end duration).
- **`logengineQueryStageDurationSeconds`** — HistogramVec labelled by `signal` and `stage` (chunk_listing, manifest_pruning, parquet_open).
- **`logengineQueryChunksListedTotal`** — CounterVec labelled by `signal`.
- **`logengineQueryChunksPrunedTotal`** — CounterVec labelled by `signal` and `reason`.
- **`logengineQueryParquetOpensTotal`** — CounterVec labelled by `signal` and `result`.
- **`logengineQueryReturnedRecordsTotal`** — CounterVec labelled by `signal`.

### Types

**`queryPathObserver`** — Fields: `signal string`, `started time.Time`. Encapsulates metrics instrumentation for a single query path.

### Functions

**`beginQueryPathObservation(signal Signal) *queryPathObserver`**

- Signature: `func beginQueryPathObservation(signal Signal) *queryPathObserver`
- Increments the queries total counter and records the start time.
- Called by: `StreamLogs`, `StreamMetrics`, `StreamTraces`, `discoverMetricNames`.

**`(*queryPathObserver).finish()`**

- Signature: `func (o *queryPathObserver) finish()`
- Records the end-to-end query duration.
- Called by: deferred from the query methods.

**`(*queryPathObserver).observeStage(stage string, started time.Time)`**

- Signature: `func (o *queryPathObserver) observeStage(stage string, started time.Time)`
- Records the duration of a sub-stage (chunk listing, manifest pruning, parquet open).
- Called by: query engine methods at each stage boundary.

**`(*queryPathObserver).addChunksListed(count int)`**

- Signature: `func (o *queryPathObserver) addChunksListed(count int)`
- Adds to the chunks listed counter (no-op if count <= 0).
- Called by: query methods after listing chunks.

**`(*queryPathObserver).addChunksPruned(reason string, count int)`**

- Signature: `func (o *queryPathObserver) addChunksPruned(reason string, count int)`
- Adds to the chunks pruned counter with a reason label.
- Called by: `logCandidates`, `metricCandidates`, `traceCandidates`.

**`(*queryPathObserver).addParquetOpen(result string)`**

- Signature: `func (o *queryPathObserver) addParquetOpen(result string)`
- Increments the parquet open counter with `"success"` or `"error"`.
- Called by: `openSignalParquet`.

**`(*queryPathObserver).addReturnedRecords(count int)`**

- Signature: `func (o *queryPathObserver) addReturnedRecords(count int)`
- Adds to the returned records counter.
- Called by: query methods at the end.

---

## storage/logengine/models.go

Package `logengine` — Data model definitions for telemetry records, chunks, and queries.

### Types

**`MetricMatchType`** — String enum: `"equal"`, `"not_equal"`, `"regexp"`, `"not_regexp"`.
**`MetricLabelMatcher`** — Fields: `Name`, `Value`, `Type`.
**`MetricQuery`** — Fields: `MetricName`, `Service`, `LabelMatchers`.
**`Signal`** — String enum: `"logs"`, `"metrics"`, `"traces"`.
**`LogRecord`** — Fields: `Timestamp`, `Level`, `Service`, `TraceID`, `RawMessage`.
**`MetricRecord`** — Fields: `Timestamp`, `Service`, `MetricName`, `Value`, `Labels map[string]string`.
**`SpanRecord`** — Fields: `Timestamp`, `EndTime`, `TraceID`, `SpanID`, `ParentSpanID`, `Service`, `Name`, `StatusCode`, `Attributes map[string]string`.
**`ChunkManifest`** — Fields: `ChunkID`, `Signal`, `MinTime`, `MaxTime`, `Services`, `MetricNames`, `MetricLabelValues`, `TraceBloom`.

No exported or unexported functions in this file — only type definitions and constants.

---

## storage/logengine/query.go

Package `logengine` — `QueryEngine` handles parsing and executing MonoFS log/metric/trace queries against the storage.

### Types

**`QueryEngine`** — Fields: `store *CachedStore`.
**`candidateChunk`** — Fields: `chunkID`, `manifest`, `cutoffMax`.
**`logRecordMinHeap`** — Min-heap of `LogRecord` (by timestamp).
**`spanRecordMinHeap`** — Min-heap of `SpanRecord` (by timestamp).
**`logChunkCursor`** — Fields: `records []LogRecord`, `index int`.
**`logChunkCursorHeap`** — Max-heap of `*logChunkCursor` (by `current().Timestamp`).
**`spanChunkCursor`** — Fields: `records []SpanRecord`, `index int`.
**`spanChunkCursorHeap`** — Max-heap of `*spanChunkCursor` (by `current().Timestamp`).
**`compiledMetricMatcher`** — Fields: `matcher *labels.Matcher`, `name string`.
**`compiledLogMatcher`** — Fields: `matcher logquery.Matcher`, `regex *regexp.Regexp`.
**`compiledLineFilter`** — Fields: `filter logquery.LineFilter`, `regex *regexp.Regexp`.
**`compiledLogQuery`** — Fields: `query logquery.Query`, `matchers`, `lineFilters`.

### Functions — QueryEngine

**`NewQueryEngine(store *CachedStore) *QueryEngine`**

- Signature: `func NewQueryEngine(store *CachedStore) *QueryEngine`
- Creates a new query engine wrapping the given cached store.
- Called by: `New` in `engine.go`.

**`(*QueryEngine).Query(ctx context.Context, queryStr string) ([]LogRecord, error)`**

- Signature: `func (q *QueryEngine) Query(ctx context.Context, queryStr string) ([]LogRecord, error)`
- Backward compatibility wrapper: calls `QueryLogs` with no filters and no limit.
- Called by: legacy query paths.

**`(*QueryEngine).QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error)`**

- Signature: `func (q *QueryEngine) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error)`
- Calls `StreamLogs` and collects results into a slice.
- Called by: `LogEngine.QueryLogs`, `Query`.

**`(*QueryEngine).StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error`**

- Signature: `func (q *QueryEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error`
- Full log query pipeline:
  1. Parses the MonoFS log query subset via `logquery.Parse`.
  2. Lists log chunk IDs from the store.
  3. Prunes candidates using `logCandidates` (time range + service via manifest).
  4. If `limit > 0`: uses a min-heap of `LogRecord` to keep top-N by timestamp. For each candidate, checks Bluge text index for line filters, scans Parquet, pushes records into heap.
  5. If no limit: uses `emitMergedLogChunks` which uses a k-way merge heap across chunk cursors.
- Called by: `LogEngine.StreamLogs`.

**`(*QueryEngine).logValidRows(ctx context.Context, chunkID string, textFilters []string) (*roaring.Bitmap, error)`**

- Signature: `func (q *QueryEngine) logValidRows(ctx context.Context, chunkID string, textFilters []string) (*roaring.Bitmap, error)`
- If no text filters, returns nil. Downloads + extracts the Bluge index for the chunk via `GetLocalIndexPath`, opens a Bluge reader, executes a Boolean query matching all text filters against `raw_message` field, returns a Roaring bitmap of matching row numbers.
- Called by: `StreamLogs`, `collectLogChunkRecords`.

**`(*QueryEngine).collectLogChunkRecords(ctx context.Context, candidate candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver) ([]LogRecord, error)`**

- Signature: `func (q *QueryEngine) collectLogChunkRecords(ctx context.Context, candidate candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver) ([]LogRecord, error)`
- Gets valid rows from Bluge, scans Parquet, collects matching records, sorts descending by timestamp.
- Called by: `emitMergedLogChunks`.

**`(*QueryEngine).emitMergedLogChunks(ctx context.Context, candidates []candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) (int, error)`**

- Signature: `func (q *QueryEngine) emitMergedLogChunks(ctx context.Context, candidates []candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) (int, error)`
- K-way merge of pre-sorted chunk cursors using a max-heap (by current record timestamp). Loads chunks lazily as needed. Yields each record in timestamp-descending order. Stops loading more candidates when their cutoff max is before the heap's current max.
- Called by: `StreamLogs`.

**`(*QueryEngine).scanLogParquet(ctx context.Context, path string, validRows *roaring.Bitmap, compiled compiledLogQuery, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) error`**

- Signature: `func (q *QueryEngine) scanLogParquet(ctx context.Context, path string, validRows *roaring.Bitmap, compiled compiledLogQuery, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) error`
- Opens the Parquet file, iterates row groups, reads all 5 columns, filters by valid rows (if Bluge bitmap provided), time range, and compiled matchers/line filters. Yields matching `LogRecord`s.
- Called by: `StreamLogs`, `collectLogChunkRecords`.

**`(*QueryEngine).logCandidates(ctx context.Context, chunkIDs []string, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`**

- Signature: `func (q *QueryEngine) logCandidates(ctx context.Context, chunkIDs []string, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`
- Reads each chunk manifest, prunes by time range and service filter. Sorts candidates by `MaxTime` descending.
- Called by: `StreamLogs`.

**`(*QueryEngine).QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`**

- Signature: `func (q *QueryEngine) QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`
- Calls `StreamMetrics` and collects results.
- Called by: `LogEngine.QueryMetrics`.

**`(*QueryEngine).StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error`**

- Signature: `func (q *QueryEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error`
- Strips discovery matchers first. If in `metric_names` discovery mode, runs `discoverMetricNames`. Otherwise lists chunk IDs, prunes candidates via manifests, scans Parquet for each candidate applying label matchers.
- Called by: `LogEngine.StreamMetrics`.

**`(*QueryEngine).discoverMetricNames(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`**

- Signature: `func (q *QueryEngine) discoverMetricNames(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error)`
- Lists metric chunks, prunes candidates via manifests, then collects unique `MetricName` values from all matching chunk manifests (without scanning Parquet). Returns lightweight `MetricRecord` entries with only the name field populated, sorted alphabetically.
- Called by: `StreamMetrics` when `__doctor_discovery__="metric_names"`.

**`(*QueryEngine).metricCandidates(ctx context.Context, chunkIDs []string, query MetricQuery, compiledMatchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`**

- Signature: `func (q *QueryEngine) metricCandidates(ctx context.Context, chunkIDs []string, query MetricQuery, compiledMatchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`
- Reads metric chunk manifests, prunes by time range, metric name, service, and label equal matchers against manifest label values.
- Called by: `StreamMetrics`, `discoverMetricNames`.

**`(*QueryEngine).scanMetricParquet(ctx context.Context, path string, query MetricQuery, matchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver, yield func(MetricRecord) error) error`**

- Signature: `func (q *QueryEngine) scanMetricParquet(ctx context.Context, path string, query MetricQuery, matchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver, yield func(MetricRecord) error) error`
- Opens metric Parquet, reads all 5 columns, filters by time range, service, metric name, and compiled label matchers (unmarshalling labels JSON per row).
- Called by: `StreamMetrics`.

**`(*QueryEngine).QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error)`**

- Signature: `func (q *QueryEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error)`
- Calls `StreamTraces` and collects results.
- Called by: `LogEngine.QueryTraces`.

**`(*QueryEngine).StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error`**

- Signature: `func (q *QueryEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error`
- Lists trace chunk IDs, prunes candidates (including trace ID Bloom filter check). With limit: uses min-heap for top-N. Without limit: uses k-way merge across chunk cursors.
- Called by: `LogEngine.StreamTraces`.

**`(*QueryEngine).traceCandidates(ctx context.Context, chunkIDs []string, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`**

- Signature: `func (q *QueryEngine) traceCandidates(ctx context.Context, chunkIDs []string, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error)`
- Reads trace chunk manifests, prunes by time range, service, and trace ID Bloom filter check.
- Called by: `StreamTraces`.

**`(*QueryEngine).collectTraceChunkRecords(ctx context.Context, candidate candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]SpanRecord, error)`**

- Signature: `func (q *QueryEngine) collectTraceChunkRecords(ctx context.Context, candidate candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]SpanRecord, error)`
- Scans trace Parquet for a single chunk, collects and sorts records.
- Called by: `emitMergedTraceChunks`.

**`(*QueryEngine).emitMergedTraceChunks(ctx context.Context, candidates []candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) (int, error)`**

- Signature: `func (q *QueryEngine) emitMergedTraceChunks(ctx context.Context, candidates []candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) (int, error)`
- K-way merge of trace chunk cursors using a max-heap.
- Called by: `StreamTraces`.

**`(*QueryEngine).scanTraceParquet(ctx context.Context, path, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) error`**

- Signature: `func (q *QueryEngine) scanTraceParquet(ctx context.Context, path, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) error`
- Opens trace Parquet, reads all 9 columns, filters by time range, trace ID, and service. Unmarshals attributes JSON per row.
- Called by: `StreamTraces`, `collectTraceChunkRecords`.

**`(*QueryEngine).openSignalParquet(ctx context.Context, path string, observer *queryPathObserver) (*file.Reader, io.ReadSeekCloser, error)`**

- Signature: `func (q *QueryEngine) openSignalParquet(ctx context.Context, path string, observer *queryPathObserver) (*file.Reader, io.ReadSeekCloser, error)`
- Reads the Parquet data via `store.remote.Read`, opens it as a Parquet reader using `openParquetReader`. Records timing and result.
- Called by: `scanLogParquet`, `scanMetricParquet`, `scanTraceParquet`.

### Package-Level Functions

**`collectQueryResults[T any](stream func(func(T) error) error) ([]T, error)`** — Generic helper: runs a stream function and collects results into a slice.
**`emitQueryResults[T any](records []T, yield func(T) error) error`** — Generic helper: yields each record from a slice.
**`compileLogQuery(query logquery.Query) (compiledLogQuery, error)`** — Compiles matchers and line filters from a parsed query, pre-compiling regexes.
**`(*compiledLogQuery).ServiceEquals() string`** — Returns the first exact `"="` service matcher.
**`(*compiledLogQuery).PositiveLineContainsFilters() []string`** — Returns all `"|="` substring filters.
**`(*compiledLogQuery).matchesRecord(record LogRecord) bool`** — Checks if a log record matches all compiled matchers and line filters.
**`logRecordField(record LogRecord, name string) (string, bool)`** — Extracts a named field from a `LogRecord` (supports: `service`, `level`/`severity_text`, `trace_id`, `body`/`raw_message`).
**`matchesLogMatcher(value string, found bool, matcher compiledLogMatcher) bool`** — Evaluates a single label matcher (`=`, `!=`, `=~`, `!~`).
**`matchesLineFilter(message string, filter compiledLineFilter) bool`** — Evaluates a single line filter (`|=`, `!=`, `|~`, `!~`).
**`buildBlugeLineFilterQuery(filters []string) bluge.Query`** — Builds a Bluge BooleanAND query from positive line filter strings against `raw_message`.
**`openParquetReader(reader io.ReadSeekCloser) (*file.Reader, error)`** — Opens a Parquet reader from a seekable reader. Returns error if not seekable.
**`compileMetricMatchers(matchers []MetricLabelMatcher) ([]compiledMetricMatcher, error)`** — Converts `MetricLabelMatcher` entries to Prometheus `labels.Matcher` instances.
**`metricMatcherTypeToProm(matchType MetricMatchType) (labels.MatchType, error)`** — Maps `MetricMatchType` to `labels.MatchType`.
**`stripMetricDiscoveryMatchers(query MetricQuery) (MetricQuery, string)`** — Removes `__doctor_discovery__` label matchers from the query and returns the cleaned query + discovery mode string.
**`manifestContains(values []string, wanted string) bool`** — Simple slice membership check.
**`manifestMatchesMetricLabels(manifest ChunkManifest, matchers []compiledMetricMatcher) bool`** — Checks if a manifest's metric label values satisfy all equal-type label matchers.
**`metricLabelsMatch(metricName, service string, values map[string]string, matchers []compiledMetricMatcher) bool`** — Checks if actual metric labels match the compiled matchers.
**`traceBloomMayContain(bloom []byte, traceID string) bool`** — Checks if a trace ID is potentially present in a Bloom filter using `bloomIndexes`.

---

## storage/logengine/s3_store.go

Package `logengine` — `S3Store` implements `ObjectStoreBackend` over AWS S3.

### Types

**`S3Store`** — Fields: `client *s3.Client`, `bucket`, `prefix`.

### Functions

**`NewS3Store(client *s3.Client, bucket, prefix string) *S3Store`**

- Signature: `func NewS3Store(client *s3.Client, bucket, prefix string) *S3Store`
- Creates a new S3Store.
- Called by: application initialization.

**`(*S3Store).fullPath(path string) string`**

- Signature: `func (s *S3Store) fullPath(path string) string`
- Prepends the configured prefix to the path. Strips leading `"/"` from path before joining. Returns path unchanged if no prefix is configured.
- Called by: `Write`, `Read`, `ListChunks`.

**`(*S3Store).Write(ctx context.Context, path string, reader io.Reader) error`**

- Signature: `func (s *S3Store) Write(ctx context.Context, path string, reader io.Reader) error`
- Calls `PutObject` to upload the reader's content to the S3 bucket.
- Called by: `Ingester` (via `CachedStore.Write`).

**`(*S3Store).Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`**

- Signature: `func (s *S3Store) Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`
- Issues `GetObject`. On `NoSuchKey` / `NotFound` error codes, returns `ErrGhostChunk`. Downloads the object body to a temp file on disk (to support seeking required by Parquet), seeks to start, and returns a `tempFileReadSeekCloser` that cleans up on close.
- Called by: `CachedStore` and `QueryEngine` when reading chunk data.

**`(*S3Store).ListChunks(ctx context.Context, prefix string) ([]string, error)`**

- Signature: `func (s *S3Store) ListChunks(ctx context.Context, prefix string) ([]string, error)`
- Uses `ListObjectsV2` paginator with `"/"` delimiter to list "directories" (chunks). Extracts the last path component of each common prefix as the chunk ID.
- Called by: `CachedStore.ListChunks`.

---

## storage/logengine/storage.go

Package `logengine` — `CachedStore` implements a caching decorator around a remote `ObjectStoreBackend` with singleflight dedup and TTL-based eviction.

### Types

**`chunkListCacheEntry`** — Fields: `chunkIDs []string`, `expiresAt time.Time`.
**`manifestCacheEntry`** — Fields: `manifest ChunkManifest`, `expiresAt time.Time`.
**`ErrGhostChunk`** — Sentinel error for missing chunks.
**`ObjectStoreBackend`** — Interface: `Write`, `Read` (returns `io.ReadSeekCloser`), `ListChunks`.
**`CachedStore`** — Fields: `remote ObjectStoreBackend`, `localDir`, `sf singleflight.Group`, `mu sync.RWMutex`, `chunks map[string]chunkListCacheEntry`, `manifests map[string]manifestCacheEntry`, `nextSweep`.

### Constants

- `chunkListCacheTTL = 10s` — How long chunk listings are cached.
- `manifestCacheTTL = 1min` — How long chunk manifests are cached.
- `cacheSweepInterval = 30s` — Minimum interval between cache purges.

### Functions

**`NewCachedStore(remote ObjectStoreBackend, localDir string) *CachedStore`**

- Signature: `func NewCachedStore(remote ObjectStoreBackend, localDir string) *CachedStore`
- Creates a new cached store with empty caches and a sweep time set to now + `cacheSweepInterval`.
- Called by: `New` in `engine.go`.

**`(*CachedStore).pruneExpiredLocked(now time.Time)`**

- Signature: `func (c *CachedStore) pruneExpiredLocked(now time.Time)`
- If the sweep interval has elapsed, removes all expired entries from `chunks` and `manifests` maps. Resets `nextSweep`.
- Called by: `ListChunks`, `ReadManifest` (under write lock).

**`(*CachedStore).Write(ctx context.Context, path string, reader io.Reader) error`**

- Signature: `func (c *CachedStore) Write(ctx context.Context, path string, reader io.Reader) error`
- Passes through to `remote.Write`, then invalidates the path's cache entries.
- Called by: `Ingester` (via `flushLogs`, `flushMetrics`, `flushTraces`).

**`(*CachedStore).ListChunks(ctx context.Context, prefix string) ([]string, error)`**

- Signature: `func (c *CachedStore) ListChunks(ctx context.Context, prefix string) ([]string, error)`
- Normalizes the prefix, checks the chunks cache (TTL 10s). On cache miss, queries the remote, sorts results, caches, and returns. Evicts stale entries before caching.
- Called by: `QueryEngine` (via `StreamLogs`, `StreamMetrics`, `StreamTraces`).

**`(*CachedStore).ReadManifest(ctx context.Context, signal Signal, chunkID string) (ChunkManifest, error)`**

- Signature: `func (c *CachedStore) ReadManifest(ctx context.Context, signal Signal, chunkID string) (ChunkManifest, error)`
- Constructs the manifest path from signal+chunkID. Checks manifest cache (TTL 1min). On miss, reads from remote, decodes JSON, caches.
- Called by: `logCandidates`, `metricCandidates`, `traceCandidates`.

**`(*CachedStore).Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`**

- Signature: `func (c *CachedStore) Read(ctx context.Context, path string) (io.ReadSeekCloser, error)`
- Only caches index files (`.index.tar.gz`). For non-index files, passes through to `remote.Read`. For index files, uses singleflight to ensure only one download per path, extracts the tar.gz to the local dir, and returns the local path. Returns an error directing callers to use `GetLocalIndexPath` for index directories.
- Called by: `QueryEngine.openSignalParquet` (for Parquet files).

**`(*CachedStore).GetLocalIndexPath(ctx context.Context, path string) (string, error)`**

- Signature: `func (c *CachedStore) GetLocalIndexPath(ctx context.Context, path string) (string, error)`
- The canonical way to get a local path to an extracted Bluge index. Uses singleflight to deduplicate downloads, extracts the tar.gz on cache miss.
- Called by: `QueryEngine.logValidRows`.

### Package-Level Functions

**`normalizeChunkPrefix(prefix string) string`**

- Signature: `func normalizeChunkPrefix(prefix string) string`
- Trims whitespace and slashes, extracts the first two path components (e.g. `"chunks/logs"`).
- Called by: `ListChunks`.

**`(*CachedStore).invalidatePath(path string)`**

- Signature: `func (c *CachedStore) invalidatePath(path string)`
- Invalidates the chunk listing cache for the path's signal prefix (e.g. `chunks/logs`). Also removes manifest cache entry if the path ends with `/metadata.json`.
- Called by: `Write`.

**`extractTarGz(r io.Reader, destDir string) error`**

- Signature: `func extractTarGz(r io.Reader, destDir string) error`
- Creates `destDir`, decompresses gzip, iterates tar entries, creates directories and extracts regular files. Preserves file modes from tar headers.
- Called by: `CachedStore.Read`, `GetLocalIndexPath`.

---

## storage/logquery/parser.go

Package `logquery` — A LogQL-compatible log query subset parser for the MonoFS logengine.

### Types

**`Matcher`** — Fields: `Name`, `Op`, `Value`. Represents a label selector (e.g. `service="foo"`).
**`LineFilter`** — Fields: `Op`, `Value`. Represents a line filter stage (e.g. `|= "error"`).
**`Query`** — Fields: `Matchers []Matcher`, `LineFilters []LineFilter`.

### Functions

**`Parse(input string) (Query, error)`**

- Signature: `func Parse(input string) (Query, error)`
- Parses a MonoFS log query string. Supports:
  - Label selectors in `{...}` (e.g. `{service="foo", level=~"error.*"}`)
  - Line filter stages: `|= "substring"`, `!= "exclude"`, `|~ "regex"`, `!~ "not_regex"`
  - Unknown pipeline stages are skipped via `skipPipelineStage`
- Returns error on empty input or unsupported fragments.
- Called by: `QueryEngine.StreamLogs`.

**`(q Query) ServiceEquals() string`**

- Signature: `func (q Query) ServiceEquals() string`
- Returns the value of the first exact `"="` matcher named `"service"`, or empty string if none.
- Called by: `compiledLogQuery.ServiceEquals()`.

**`(q Query) PositiveLineContainsFilters() []string`**

- Signature: `func (q Query) PositiveLineContainsFilters() []string`
- Returns the values of all `"|="` (substring contains) line filter operators.
- Called by: `compiledLogQuery.PositiveLineContainsFilters()`.

### Package-Level (Unexported) Functions

**`parseSelector(input string) ([]Matcher, string, error)`** — Parses `{...}` selector block. Extracts individual matcher expressions separated by commas, respecting quoted strings. Returns matchers and remaining input after `}`.

**`findSelectorEnd(input string) (int, error)`** — Finds the closing `}` of a selector block, respecting quoted strings and escape sequences.

**`splitOutsideQuotes(input string, separator byte) ([]string, error)`** — Splits a string on a delimiter, but only outside of quoted sections.

**`parseMatcher(input string) (Matcher, error)`** — Parses a single `name=op"value"` matcher. Supports operators `=~`, `!~`, `!=`, `=`. Value must be a quoted string.

**`parseLineFilter(input string) (LineFilter, string, bool, error)`** — Attempts to parse a line filter operator (`|=`, `!=`, `|~`, `!~`) from the start of input. Returns the filter, remaining input, a bool indicating success, and any error.

**`consumeQuotedString(input string) (string, string, error)`** — Consumes a `"..."`, `'...'`, or `` `...` `` quoted string from input. Returns the unquoted value, remaining input, and error.

**`skipPipelineStage(input string) (string, error)`** — Skips an unknown pipeline stage (anything between `|` and the next recognized operator or end of string), respecting brackets and quotes. Returns the remaining input after the skipped stage.

**`startsLineFilterAt(input string, index int) bool`** — Returns true if a line filter operator starts at the given index in the string.

---

## storage/workspacestore/store.go

Package `workspacestore` — The main workspace sync state store, managing jobs, bundles, audit events, and a ledger backed by a WAL with periodic compaction.

### Types

**`Store`** — Fields: `cfg StoreConfig`, `logger *slog.Logger`, `mu sync.RWMutex`, `nextSeq uint64`, `jobs map[string]*jobEntry`, `bundles map[string]*BundleMetadata`, `auditEvents []*AuditEvent`, `ledgerEntries [][]byte`, `wal *walWriter`, `compactMu sync.Mutex`, `stopCompact chan struct{}`, `checkpoint *Checkpoint`.

### Functions

**`New(cfg StoreConfig, logger *slog.Logger) (*Store, error)`**

- Signature: `func New(cfg StoreConfig, logger *slog.Logger) (*Store, error)`
- Initializes a Store. If `StateDir` is empty, operates in memory-only mode. Otherwise: creates state dir, loads checkpoint, creates WAL writer, replays WAL entries, starts compaction loop goroutine.
- Called by: workspace service initialization.

**`(*Store).UpsertJob(job *pb.WorkspaceSyncJob) error`**

- Signature: `func (s *Store) UpsertJob(job *pb.WorkspaceSyncJob) error`
- JSON-marshals the protobuf job, acquires write lock, increments `nextSeq`, clones the proto message, stores it in the `jobs` map by job ID, constructs a WAL entry with `OpUpsert`/`KindJob`, releases lock, appends to WAL.
- Called by: workspace sync API handlers.

**`(*Store).GetJob(jobID string) *pb.WorkspaceSyncJob`**

- Signature: `func (s *Store) GetJob(jobID string) *pb.WorkspaceSyncJob`
- Returns a snapshot (proto clone) of the job, or nil if not found. Thread-safe (read lock).
- Called by: API query handlers.

**`(*Store).ListJobs(filter func(*pb.WorkspaceSyncJob) bool) []*pb.WorkspaceSyncJob`**

- Signature: `func (s *Store) ListJobs(filter func(*pb.WorkspaceSyncJob) bool) []*pb.WorkspaceSyncJob`
- Returns snapshots of all jobs that pass the optional filter function.
- Called by: API listing handlers.

**`(*Store).JobCount() int`**

- Signature: `func (s *Store) JobCount() int`
- Returns the number of jobs in memory.
- Called by: status endpoints.

**`(*Store).UpsertBundle(b *BundleMetadata) error`**

- Signature: `func (s *Store) UpsertBundle(b *BundleMetadata) error`
- JSON-marshals the bundle metadata, acquires lock, copies and stores in `bundles` map by `BundleID`, creates a WAL entry with `OpUpsert`/`KindBundle`.
- Called by: bundle tracking API.

**`(*Store).GetBundle(bundleID string) *BundleMetadata`**

- Signature: `func (s *Store) GetBundle(bundleID string) *BundleMetadata`
- Returns a pointer to the stored bundle metadata (not a copy — caller should not mutate).
- Called by: API query handlers.

**`(*Store).InsertAudit(event *AuditEvent) error`**

- Signature: `func (s *Store) InsertAudit(event *AuditEvent) error`
- JSON-marshals the audit event, acquires lock, copies and appends to `auditEvents` slice, assigns a sequence number, creates WAL entry with `OpInsert`/`KindAudit`.
- Called by: audit trail recording.

**`(*Store).ListAuditEvents() []*AuditEvent`**

- Signature: `func (s *Store) ListAuditEvents() []*AuditEvent`
- Returns a deep copy of all audit events.
- Called by: audit query handlers.

**`(*Store).InsertLedger(data []byte) error`**

- Signature: `func (s *Store) InsertLedger(data []byte) error`
- Acquires lock, appends a copy of the data to `ledgerEntries`, creates WAL entry with `OpInsert`/`KindLedger`.
- Called by: ledger recording.

**`(*Store).ReplayLedgerEntries(callback func([]byte) error) error`**

- Signature: `func (s *Store) ReplayLedgerEntries(callback func([]byte) error) error`
- Returns deep copies of all ledger entries and invokes the callback on each. Returns error if any callback fails.
- Called by: ledger consumers (e.g. Doctor Partition).

**`(*Store).Close()`**

- Signature: `func (s *Store) Close()`
- Closes the compaction stop channel, performs a final compaction if a WAL is active, closes the WAL.
- Called by: graceful shutdown.

**`(*Store).loadCheckpoint() error`**

- Signature: `func (s *Store) loadCheckpoint() error`
- Reads `checkpoints/checkpoint.json` from the state dir. If the file doesn't exist, returns nil. Corrupt files are logged and ignored.
- Called by: `New`.

**`(*Store).recover() error`**

- Signature: `func (s *Store) recover() error`
- If a checkpoint exists, loads the snapshot from the checkpoint file. Then replays WAL entries with sequence numbers above the checkpointed `LastCompactedSeq`. Skips corrupt entries with a warning. Updates `nextSeq` to be above all replayed entries.
- Called by: `New`.

**`(*Store).compactLoop()`**

- Signature: `func (s *Store) compactLoop()`
- Runs a ticker at `cfg.CompactionInterval`. On each tick, calls `compact()`. Stops when `stopCompact` is closed.
- Called by: `New` (spawned as goroutine).

**`(*Store).compact() error`**

- Signature: `func (s *Store) compact() error`
- Checks WAL total size against `CompactionSizeThreshold`. If below threshold, returns nil. Takes a read lock to snapshot all jobs, bundles, audit events, and ledger entries. Writes the snapshot to a JSON file, writes an atomic checkpoint file (via temp+rename), deletes old WAL segments that are fully below the checkpoint sequence, and cleans up old snapshot files.
- Called by: `compactLoop`, `Close`.

**`(*Store).loadSnapshot() error`**

- Signature: `func (s *Store) loadSnapshot() error`
- Reads the snapshot file referenced by the checkpoint, unmarshals its JSON, and populates the in-memory maps/slices (jobs, bundles, audit events, ledger entries).
- Called by: `recover`.

**`(*Store).writeSnapshot(snapshot *compactedSnapshot) (string, error)`**

- Signature: `func (s *Store) writeSnapshot(snapshot *compactedSnapshot) (string, error)`
- JSON-encodes the snapshot to a temp file under `checkpoints/`, atomically renames to `snapshot-<seq>.json`. Returns the filename.
- Called by: `compact`.

**`(*Store).cleanupOldSnapshots(currentSnapshotFile string)`**

- Signature: `func (s *Store) cleanupOldSnapshots(currentSnapshotFile string)`
- Globs `snapshot-*.json` in the checkpoints directory, removes all except the current one.
- Called by: `compact`.

**`(*Store).statePath(parts ...string) string`**

- Signature: `func (s *Store) statePath(parts ...string) string`
- Joins `StateDir` with the given path parts using `"/"`.
- Called by: internal path construction throughout the store.

---

## storage/workspacestore/types.go

Package `workspacestore` — Type definitions, constants, and helper functions.

### Types

**`EntityKind`** — String enum: `"JOB"`, `"BUNDLE"`, `"AUDIT"`, `"LEDGER"`.
**`WalOp`** — String enum: `"UPSERT"`, `"INSERT"`.
**`WALEntry`** — Fields: `Seq uint64`, `TS time.Time`, `Op WalOp`, `Kind EntityKind`, `Data json.RawMessage`.
**`BundleMetadata`** — Fields: `BundleID`, `WorkspaceID`, `Kind`, `ByteSize`, `RepoCount`, `LocalCommitIDs`, `CreatedAtUnix`, `ExpiresAtUnix`, `DiscardReason`, `JobID`.
**`AuditEvent`** — Fields: `WorkspaceID`, `JobID`, `BundleID`, `LocalCommitID`, `ActorPrincipalID`, `Decision`, `ReasonCode`, `Timestamp`, `CorrelationID`, `Seq` (unexported from JSON).
**`Checkpoint`** — Fields: `LastCompactedSeq uint64`, `SnapshotFile string`.
**`compactedSnapshot`** — Fields: `Version`, `CreatedAtUnix`, `CheckpointSeq`, `Jobs`, `Bundles`, `AuditEvents`, `LedgerEntries`.
**`jobSnapshot`** — Fields: `Data []byte`.
**`StoreConfig`** — Fields: `StateDir`, `CompactionInterval`, `CompactionSizeThreshold`, `JobRetentionDays`, `MaxJobsPerWorkspace`, `LocalRetentionDays`, `FsyncEnabled`.
**`jobEntry`** — Fields: `mu sync.RWMutex`, `job *pb.WorkspaceSyncJob`.

### Functions

**`DefaultStoreConfig(stateDir string) StoreConfig`**

- Signature: `func DefaultStoreConfig(stateDir string) StoreConfig`
- Returns a config with sensible defaults: compaction every 5 minutes, 256 MB threshold, 30-day retention, 1000 max jobs per workspace, fsync disabled.
- Called by: workspace service initialization.

**`(*jobEntry).snapshot() *pb.WorkspaceSyncJob`**

- Signature: `func (e *jobEntry) snapshot() *pb.WorkspaceSyncJob`
- Returns a proto-cloned copy of the job, read-locked.
- Called by: `GetJob`, `ListJobs`, `compact`.

**`auditEventsByTimestamp(audit []*AuditEvent)`** — Sorts audit events by timestamp ascending (in-place). Usage TBD.

**`jobIndexKey(jobID string) string`** — Returns `"job:<jobID>"`. Usage TBD.

**`bundleIndexKey(bundleID string) string`** — Returns `"bundle:<bundleID>"`. Usage TBD.

**`auditIndexKey(jobID, timestamp string) string`** — Returns `"audit:<jobID>:<timestamp>"`. Usage TBD.

---

## storage/workspacestore/wal.go

Package `workspacestore` — Write-Ahead Log (WAL) implementation with segmented files, replay, and compaction support.

### Types

**`walWriter`** — Fields: `dir`, `mu sync.Mutex`, `logger *slog.Logger`, `activeFile *os.File`, `segmentIdx uint64`, `segmentSize int64`, `fsync bool`.

### Constants

- `maxSegmentSize = 64MB` — Maximum size per WAL segment file before rotation.

### Functions

**`newWALWriter(dir string, logger *slog.Logger, fsync bool) (*walWriter, error)`**

- Signature: `func newWALWriter(dir string, logger *slog.Logger, fsync bool) (*walWriter, error)`
- Creates the WAL directory under `dir/wal`. Scans existing segments, uses the highest index as the starting segment, opens or creates that segment file.
- Called by: `New` in `store.go`.

**`(*walWriter).segmentPath(idx uint64) string`**

- Signature: `func (w *walWriter) segmentPath(idx uint64) string`
- Returns the path for a WAL segment: `wal/wal-00000N.log`.
- Called by: internal WAL methods.

**`(*walWriter).openSegment(idx uint64, create bool) error`**

- Signature: `func (w *walWriter) openSegment(idx uint64, create bool) error`
- Closes current active file, opens the segment at `idx` (with `O_CREATE` if requested), stats it to get current size. Updates `activeFile`, `segmentIdx`, `segmentSize`.
- Called by: `newWALWriter`, `rotateSegmentLocked`.

**`(*walWriter).Append(entry WALEntry) error`**

- Signature: `func (w *walWriter) Append(entry WALEntry) error`
- JSON-marshals the entry, appends a newline. If appending would exceed `maxSegmentSize`, calls `rotateSegmentLocked` first. Writes to the active file. Optionally calls `Sync()` if `fsync` is enabled.
- Called by: `Store.UpsertJob`, `UpsertBundle`, `InsertAudit`, `InsertLedger`.

**`(*walWriter).rotateSegmentLocked() error`**

- Signature: `func (w *walWriter) rotateSegmentLocked() error`
- Closes the current active file, increments `segmentIdx`, opens the next segment in create mode. Must hold `mu` lock.
- Called by: `Append`.

**`(*walWriter).TotalSize() int64`**

- Signature: `func (w *walWriter) TotalSize() int64`
- Iterates all segment files on disk, sums their sizes. Thread-safe (locks).
- Called by: `Store.compact` to determine if compaction is needed.

**`(*walWriter).listSegments() []uint64`**

- Signature: `func (w *walWriter) listSegments() []uint64`
- Reads the WAL directory, parses segment indices from filenames matching `wal-*.log`, returns sorted ascending indices.
- Called by: `newWALWriter`, `TotalSize`, `ReplayEntries`, `DeleteSegmentsBelow`.

**`(*walWriter).ReplayEntries(aboveSeq uint64) ([]WALEntry, error)`**

- Signature: `func (w *walWriter) ReplayEntries(aboveSeq uint64) ([]WALEntry, error)`
- Reads all WAL segments, parses entries from each, collects entries with `entry.Seq > aboveSeq`. Sorts results by sequence number. Tolerates corrupt lines and scanner-recoverable errors (`ErrFinalToken`, `ErrTooLong`).
- Called by: `Store.recover`.

**`(*walWriter).DeleteSegmentsBelow(belowSeq uint64) error`**

- Signature: `func (w *walWriter) DeleteSegmentsBelow(belowSeq uint64) error`
- Reads each non-active segment, checks if all entries have `Seq <= belowSeq`. If so, deletes the segment file. Active segment is never deleted.
- Called by: `Store.compact` after successful checkpoint.

**`(*walWriter).Close() error`**

- Signature: `func (w *walWriter) Close() error`
- Closes the active file and clears the reference.
- Called by: `Store.Close`.

**`isScannerRecoverable(err error) bool`**

- Signature: `func isScannerRecoverable(err error) bool`
- Returns `true` for `bufio.ErrFinalToken` or `bufio.ErrTooLong`, which are recoverable during WAL replay.
- Called by: `ReplayEntries`.

**`readSegmentEntries(path string) ([]WALEntry, error)`**

- Signature: `func readSegmentEntries(path string) ([]WALEntry, error)`
- Opens a WAL segment file, scans it line-by-line with a 1 MB initial / 16 MB max buffer, unmarshals each line as a `WALEntry`. Skips empty lines and corrupt entries silently. Returns both parsed entries and scanner error (which may be recoverable).
- Called by: `ReplayEntries`, `DeleteSegmentsBelow`.
