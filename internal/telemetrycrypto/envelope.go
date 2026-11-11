package telemetrycrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var envelopeMagic = [9]byte{'S', 'T', 'R', 'A', 'T', 'A', 'E', 'N', 'C'}

const (
	envelopeVersion = byte(1)
	maxHeaderField  = 64 * 1024
	fieldPrefix     = "enc:v1:"
)

// IsEnvelope reports whether data begins with the telemetry envelope magic.
func IsEnvelope(data []byte) bool {
	if len(data) < len(envelopeMagic) {
		return false
	}
	for i := range envelopeMagic {
		if data[i] != envelopeMagic[i] {
			return false
		}
	}
	return true
}

// Seal encrypts plaintext with the tenant/partition subkey. context is bound
// into the AEAD tag; callers should pass the storage path or a stable field
// name.
func (p *Provider) Seal(plain []byte, tenant, partition, context string) ([]byte, error) {
	if p == nil || !p.enabled {
		return append([]byte(nil), plain...), nil
	}
	keyID, key, err := p.KeyFor(tenant, partition)
	if err != nil {
		return nil, err
	}
	tenant = normalizeTenant(p, tenant)
	partition = normalizePartition(partition)
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("telemetrycrypto: nonce: %w", err)
	}
	aad := aadFor(context, tenant, partition, keyID)
	sealed := aead.Seal(nil, nonce, plain, aad)

	headerLen := len(envelopeMagic) + 1 + 8 + len(keyID) + len(tenant) + len(partition) + len(nonce)
	out := make([]byte, 0, headerLen+len(sealed))
	out = append(out, envelopeMagic[:]...)
	out = append(out, envelopeVersion)
	out = appendUint16(out, len(keyID))
	out = appendUint16(out, len(tenant))
	out = appendUint16(out, len(partition))
	out = appendUint16(out, len(nonce))
	out = append(out, keyID...)
	out = append(out, tenant...)
	out = append(out, partition...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Open decrypts an envelope. Plaintext is returned unchanged in observe mode
// and rejected in enforce mode.
func (p *Provider) Open(data []byte, context string) ([]byte, error) {
	if !IsEnvelope(data) {
		if p != nil && p.Mode() == ModeEnforce {
			return nil, ErrPlaintextRejected
		}
		return append([]byte(nil), data...), nil
	}
	header, ciphertext, err := parseEnvelope(data)
	if err != nil {
		return nil, err
	}
	keyID, key, err := p.KeyFor(header.tenant, header.partition)
	if err != nil {
		return nil, err
	}
	if keyID != header.keyID {
		return nil, fmt.Errorf("telemetrycrypto: envelope key id %q does not match partition key id %q", header.keyID, keyID)
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(header.nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("telemetrycrypto: invalid nonce size %d", len(header.nonce))
	}
	aad := aadFor(context, header.tenant, header.partition, header.keyID)
	plain, err := aead.Open(nil, header.nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("telemetrycrypto: decrypt: %w", err)
	}
	return plain, nil
}

// EncryptField encodes a single string column value. The result is safe to
// store in an existing String column.
func (p *Provider) EncryptField(plain, tenant, partition, field string) (string, error) {
	if p == nil || !p.enabled {
		return plain, nil
	}
	sealed, err := p.Seal([]byte(plain), tenant, partition, "field:"+field)
	if err != nil {
		return "", err
	}
	return fieldPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// DecryptField reverses EncryptField.
func (p *Provider) DecryptField(value, field string) (string, error) {
	if !strings.HasPrefix(value, fieldPrefix) {
		if p != nil && p.Mode() == ModeEnforce {
			return "", ErrPlaintextRejected
		}
		return value, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, fieldPrefix))
	if err != nil {
		return "", fmt.Errorf("telemetrycrypto: decode field: %w", err)
	}
	plain, err := p.Open(raw, "field:"+field)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// IsEncryptedField reports whether a column value carries the field envelope.
func IsEncryptedField(value string) bool {
	return strings.HasPrefix(value, fieldPrefix)
}

type envelopeHeader struct {
	keyID     string
	tenant    string
	partition string
	nonce     []byte
}

func parseEnvelope(data []byte) (envelopeHeader, []byte, error) {
	var header envelopeHeader
	if !IsEnvelope(data) {
		return header, nil, errors.New("telemetrycrypto: not an envelope")
	}
	offset := len(envelopeMagic)
	if len(data) < offset+1+8 {
		return header, nil, errors.New("telemetrycrypto: truncated envelope")
	}
	if data[offset] != envelopeVersion {
		return header, nil, fmt.Errorf("telemetrycrypto: unsupported envelope version %d", data[offset])
	}
	offset++
	lengths := make([]int, 4)
	for i := range lengths {
		lengths[i] = int(binary.BigEndian.Uint16(data[offset:]))
		offset += 2
		if lengths[i] < 0 || lengths[i] > maxHeaderField {
			return header, nil, errors.New("telemetrycrypto: invalid envelope field length")
		}
	}
	total := 0
	for _, length := range lengths {
		total += length
	}
	if len(data) < offset+total {
		return header, nil, errors.New("telemetrycrypto: truncated envelope header")
	}
	take := func(length int) []byte {
		value := data[offset : offset+length]
		offset += length
		return value
	}
	header.keyID = string(take(lengths[0]))
	header.tenant = string(take(lengths[1]))
	header.partition = string(take(lengths[2]))
	header.nonce = append([]byte(nil), take(lengths[3])...)
	return header, data[offset:], nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("telemetrycrypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("telemetrycrypto: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("telemetrycrypto: gcm: %w", err)
	}
	return aead, nil
}

func appendUint16(dst []byte, value int) []byte {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], uint16(value))
	return append(dst, buf[:]...)
}

func aadFor(context, tenant, partition, keyID string) []byte {
	return []byte(context + "|" + tenant + "|" + partition + "|" + keyID)
}

func normalizeTenant(p *Provider, tenant string) string {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		if p != nil {
			return p.Tenant()
		}
		return DefaultTenant
	}
	return tenant
}

func normalizePartition(partition string) string {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return UnpartitionedKey
	}
	return partition
}
