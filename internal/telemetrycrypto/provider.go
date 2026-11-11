// Package telemetrycrypto implements the optional per-partition telemetry
// encryption layer used by the MonoFS logengine and ClickHouse backends.
//
// The hierarchy is intentionally small:
//
//	tenant root key
//	  └── partition subkey (explicit, or HKDF-derived from the root)
//	        └── per-object / per-batch data key
//
// Only the data key is stored next to the ciphertext. The root and partition
// keys never leave the process.
package telemetrycrypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Mode controls what the reader does when it encounters plaintext.
type Mode string

const (
	// ModeObserve writes encrypted data but still reads legacy plaintext.
	ModeObserve Mode = "observe"
	// ModeEnforce rejects plaintext telemetry.
	ModeEnforce Mode = "enforce"
)

const (
	// DefaultTenant is used when no tenant is configured or supplied.
	DefaultTenant = "default"
	// UnpartitionedKey is the subkey used for records without a partition.
	UnpartitionedKey = "_unpartitioned"
	// RootKeyVersion is the default key ID when the key file omits one.
	RootKeyVersion = "v1"
)

// ErrPlaintextRejected is returned in enforce mode when a reader sees an
// object or field that is not encrypted.
var ErrPlaintextRejected = errors.New("telemetry encryption is enforced but plaintext was found")

// Config describes the key material and reader policy.
type Config struct {
	// Enabled turns write encryption on. Disabled providers never encrypt new
	// data unless DecryptOnly is set (which still enables reads).
	Enabled bool
	// DecryptOnly loads keys for reading existing envelopes while new writes
	// stay plaintext. It is ignored when Enabled is true.
	DecryptOnly bool
	// Mode controls plaintext handling. Empty defaults to observe.
	Mode Mode
	// Tenant is the deployment-level tenant when one is not supplied.
	Tenant string
	// KeysFile and KeysJSON are alternative sources. KeysJSON wins.
	KeysFile string
	KeysJSON string
	// RootKeyID and RootKey are populated from the key file.
	RootKeyID string
	RootKey   []byte
	// Partitions holds explicit per-partition subkeys.
	Partitions map[string]PartitionKey
}

// PartitionKey is an explicit subkey for one partition.
type PartitionKey struct {
	KeyID string `json:"key_id,omitempty"`
	Key   []byte `json:"-"`
}

type keyFile struct {
	Version    int                    `json:"version"`
	Tenant     string                 `json:"tenant,omitempty"`
	RootKeyID  string                 `json:"root_key_id,omitempty"`
	RootKey    string                 `json:"root_key,omitempty"`
	Partitions map[string]keyFilePart `json:"partitions,omitempty"`
}

type keyFilePart struct {
	KeyID string `json:"key_id,omitempty"`
	Key   string `json:"key,omitempty"`
}

// Provider resolves partition keys and seals/opens telemetry values.
type Provider struct {
	enabled     bool
	decryptOnly bool
	mode        Mode
	tenant      string
	rootKeyID   string
	rootKey     []byte
	partitions  map[string]PartitionKey

	mu    sync.RWMutex
	cache map[string]cachedKey
}

type cachedKey struct {
	keyID string
	key   []byte
}

// NewProvider builds a provider from config and validates the key material.
// A disabled provider is valid and never encrypts.
func NewProvider(cfg Config) (*Provider, error) {
	p := &Provider{
		enabled:     cfg.Enabled,
		decryptOnly: cfg.DecryptOnly && !cfg.Enabled,
		mode:        cfg.Mode,
		tenant:      strings.TrimSpace(cfg.Tenant),
		rootKeyID:   strings.TrimSpace(cfg.RootKeyID),
		rootKey:     append([]byte(nil), cfg.RootKey...),
		partitions:  map[string]PartitionKey{},
		cache:       map[string]cachedKey{},
	}
	if p.mode == "" {
		p.mode = ModeObserve
	}
	if p.mode != ModeObserve && p.mode != ModeEnforce {
		return nil, fmt.Errorf("telemetrycrypto: unsupported mode %q (want observe or enforce)", p.mode)
	}
	if p.tenant == "" {
		p.tenant = DefaultTenant
	}
	for name, key := range cfg.Partitions {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(key.Key) != 32 {
			return nil, fmt.Errorf("telemetrycrypto: partition %q key must be 32 bytes, got %d", name, len(key.Key))
		}
		p.partitions[name] = PartitionKey{KeyID: strings.TrimSpace(key.KeyID), Key: append([]byte(nil), key.Key...)}
	}
	if p.enabled || p.decryptOnly || len(p.rootKey) > 0 || len(p.partitions) > 0 {
		if len(p.rootKey) > 0 && len(p.rootKey) != 32 {
			return nil, fmt.Errorf("telemetrycrypto: root key must be 32 bytes, got %d", len(p.rootKey))
		}
		if (p.enabled || p.decryptOnly) && len(p.rootKey) == 0 && len(p.partitions) == 0 {
			return nil, errors.New("telemetrycrypto: envelope encryption requires a root key or explicit partition keys")
		}

		if p.rootKeyID == "" {
			p.rootKeyID = RootKeyVersion
		}
	}
	return p, nil
}

// Setting describes whether telemetry encryption is enabled for writes and/or
// reads. "read" allows a deployment to stop encrypting new data while still
// decrypting existing envelopes.
type Setting struct {
	Write bool
	Read  bool
}

// ParseSetting parses the MONOFS_TELEMETRY_ENCRYPTION style value.
// Accepted values: off, read/decrypt, envelope/on.
func ParseSetting(value string) (Setting, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off", "false", "disabled":
		return Setting{}, nil
	case "read", "decrypt", "decrypt-only", "read-only":
		return Setting{Read: true}, nil
	case "envelope", "on", "true", "enabled":
		return Setting{Write: true, Read: true}, nil
	default:
		return Setting{}, fmt.Errorf("telemetrycrypto: unsupported encryption setting %q (want off, read, or envelope)", value)
	}
}

// LoadProvider reads KeysFile or KeysJSON when configured. keysJSON wins over
// keysFile so the demo can patch a deployment without mounting a volume.
func LoadProvider(cfg Config) (*Provider, error) {
	raw := strings.TrimSpace(cfg.KeysJSON)
	if raw == "" && strings.TrimSpace(cfg.KeysFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.KeysFile))
		if err != nil {
			return nil, fmt.Errorf("telemetrycrypto: read key file: %w", err)
		}
		raw = string(data)
	}
	if strings.TrimSpace(raw) != "" {
		parsed, err := ParseKeyFile([]byte(raw))
		if err != nil {
			return nil, err
		}
		// The key file always advertises Enabled; the flags are authoritative
		// so "read" and "off" are not silently upgraded.
		if cfg.Mode == "" {
			cfg.Mode = parsed.Mode
		}
		if strings.TrimSpace(cfg.Tenant) == "" {
			cfg.Tenant = parsed.Tenant
		}
		if strings.TrimSpace(cfg.RootKeyID) == "" {
			cfg.RootKeyID = parsed.RootKeyID
		}
		if len(cfg.RootKey) == 0 {
			cfg.RootKey = parsed.RootKey
		}
		if len(cfg.Partitions) == 0 {
			cfg.Partitions = parsed.Partitions
		}
	}
	return NewProvider(cfg)
}

// ParseKeyFile parses a key-file document. It does not enable encryption by
// itself; callers decide whether the feature is on.
func ParseKeyFile(data []byte) (Config, error) {
	var doc keyFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return Config{}, fmt.Errorf("telemetrycrypto: parse key file: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return Config{}, fmt.Errorf("telemetrycrypto: unsupported key file version %d", doc.Version)
	}
	cfg := Config{
		Enabled:    true,
		Mode:       ModeObserve,
		Tenant:     strings.TrimSpace(doc.Tenant),
		RootKeyID:  strings.TrimSpace(doc.RootKeyID),
		Partitions: map[string]PartitionKey{},
	}
	if cfg.Tenant == "" {
		cfg.Tenant = DefaultTenant
	}
	if strings.TrimSpace(doc.RootKey) != "" {
		key, err := decodeKey(doc.RootKey)
		if err != nil {
			return Config{}, fmt.Errorf("telemetrycrypto: root key: %w", err)
		}
		cfg.RootKey = key
	}
	for name, part := range doc.Partitions {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key, err := decodeKey(part.Key)
		if err != nil {
			return Config{}, fmt.Errorf("telemetrycrypto: partition %q key: %w", name, err)
		}
		cfg.Partitions[name] = PartitionKey{KeyID: strings.TrimSpace(part.KeyID), Key: key}
	}
	return cfg, nil
}

func decodeKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("empty key")
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		// Accept unpadded base64 as a convenience for generated demo keys.
		key, err = base64.RawStdEncoding.DecodeString(value)
		if err != nil {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// Enabled reports whether new values are encrypted.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// DecryptOnly reports that the provider reads envelopes but does not encrypt
// new data.
func (p *Provider) DecryptOnly() bool { return p != nil && p.decryptOnly }

// Active reports whether the provider participates in storage at all. It is
// true when it can write or read encrypted data.
func (p *Provider) Active() bool { return p != nil && (p.enabled || p.decryptOnly) }

// Mode returns the plaintext policy.
func (p *Provider) Mode() Mode {
	if p == nil || p.mode == "" {
		return ModeObserve
	}
	return p.mode
}

// Tenant returns the default tenant.
func (p *Provider) Tenant() string {
	if p == nil || p.tenant == "" {
		return DefaultTenant
	}
	return p.tenant
}

// RootKeyID returns the configured root key version.
func (p *Provider) RootKeyID() string {
	if p == nil || p.rootKeyID == "" {
		return RootKeyVersion
	}
	return p.rootKeyID
}

// KeyFor returns the subkey and stable key ID for a tenant/partition.
func (p *Provider) KeyFor(tenant, partition string) (string, []byte, error) {
	if p == nil || (!p.enabled && !p.decryptOnly) {
		return "", nil, errors.New("telemetrycrypto: encryption is disabled")
	}
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		tenant = p.Tenant()
	}
	partition = strings.TrimSpace(partition)
	if partition == "" {
		partition = UnpartitionedKey
	}
	cacheKey := tenant + "\x00" + partition

	p.mu.RLock()
	if entry, ok := p.cache[cacheKey]; ok {
		p.mu.RUnlock()
		return entry.keyID, append([]byte(nil), entry.key...), nil
	}
	p.mu.RUnlock()

	explicit, ok := p.partitions[tenant+"/"+partition]
	if !ok {
		explicit, ok = p.partitions[partition]
	}
	if ok && len(explicit.Key) == 32 {
		keyID := explicit.KeyID
		if keyID == "" {
			keyID = tenant + "/" + partition + "/" + p.RootKeyID()
		}
		p.mu.Lock()
		p.cache[cacheKey] = cachedKey{keyID: keyID, key: append([]byte(nil), explicit.Key...)}
		p.mu.Unlock()
		return keyID, append([]byte(nil), explicit.Key...), nil
	}
	if len(p.rootKey) != 32 {
		return "", nil, fmt.Errorf("telemetrycrypto: no key configured for partition %q", partition)
	}
	key := hkdfSHA256(p.rootKey, []byte("strata-telemetry-v1"), []byte(tenant+"/"+partition), 32)
	keyID := tenant + "/" + partition + "/" + p.RootKeyID()
	p.mu.Lock()
	p.cache[cacheKey] = cachedKey{keyID: keyID, key: append([]byte(nil), key...)}
	p.mu.Unlock()
	return keyID, key, nil
}

// hkdfSHA256 is a small RFC 5869 HKDF-SHA256 implementation so the storage
// packages do not depend on x/crypto.
func hkdfSHA256(ikm, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)

	out := make([]byte, 0, length)
	var block []byte
	for counter := byte(1); len(out) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		expand.Write(block)
		expand.Write(info)
		expand.Write([]byte{counter})
		block = expand.Sum(nil)
		out = append(out, block...)
	}
	return out[:length]
}
