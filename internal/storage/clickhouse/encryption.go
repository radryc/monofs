package clickhouse

import (
	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/telemetrycrypto"
)

func (s *TraceStore) encryption() *telemetrycrypto.Provider {
	if s == nil || s.cfg.Encryption == nil || !s.cfg.Encryption.Active() {
		return nil
	}
	return s.cfg.Encryption
}

func (s *TraceStore) tenantName() string {
	if s == nil || s.cfg.Tenant == "" {
		return "default"
	}
	return s.cfg.Tenant
}

func (s *TraceStore) encryptField(partition, field, value string) (string, error) {
	enc := s.encryption()
	if enc == nil || value == "" {
		return value, nil
	}
	return enc.EncryptField(value, s.tenantName(), partition, field)
}

func (s *TraceStore) decryptField(value, field string) (string, error) {
	enc := s.encryption()
	if enc == nil || value == "" {
		return value, nil
	}
	return enc.DecryptField(value, field)
}

func partitionFor(labels map[string]string) string {
	return logengine.PartitionFromLabels(labels)
}
