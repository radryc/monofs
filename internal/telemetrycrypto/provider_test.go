package telemetrycrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testKey(t *testing.T, fill byte) string {
	t.Helper()
	key := bytes.Repeat([]byte{fill}, 32)
	return base64.StdEncoding.EncodeToString(key)
}

func TestDerivedPartitionKeysAreStableAndDistinct(t *testing.T) {
	cfg, err := ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "acme",
		"root_key_id": "root-v1",
		"root_key": "` + testKey(t, 1) + `"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	idA, keyA, err := p.KeyFor("acme", "diva-shared")
	if err != nil {
		t.Fatal(err)
	}
	idB, keyB, err := p.KeyFor("acme", "diva-shared")
	if err != nil {
		t.Fatal(err)
	}
	if idA != idB || !bytes.Equal(keyA, keyB) {
		t.Fatal("partition key is not stable")
	}
	idC, keyC, err := p.KeyFor("acme", "divb-shared")
	if err != nil {
		t.Fatal(err)
	}
	if idA == idC || bytes.Equal(keyA, keyC) {
		t.Fatal("different partitions must get different subkeys")
	}
	if !strings.HasPrefix(idA, "acme/diva-shared/") {
		t.Fatalf("key id %q does not contain tenant/partition", idA)
	}
}

func TestExplicitPartitionKeyWins(t *testing.T) {
	cfg, err := ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "acme",
		"root_key": "` + testKey(t, 1) + `",
		"partitions": {
			"diva-shared": {"key_id": "custom/key", "key": "` + testKey(t, 9) + `"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, key, err := p.KeyFor("acme", "diva-shared")
	if err != nil {
		t.Fatal(err)
	}
	if id != "custom/key" {
		t.Fatalf("key id = %q, want custom/key", id)
	}
	if want := bytes.Repeat([]byte{9}, 32); !bytes.Equal(key, want) {
		t.Fatal("explicit partition key was not used")
	}
}

func TestTenantPrefixedExplicitPartitionKey(t *testing.T) {
	cfg, err := ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "acme",
		"partitions": {
			"acme/diva-shared": {"key_id": "acme/diva-shared/custom", "key": "` + testKey(t, 11) + `"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, key, err := p.KeyFor("acme", "diva-shared")
	if err != nil {
		t.Fatal(err)
	}
	if id != "acme/diva-shared/custom" {
		t.Fatalf("key id = %q", id)
	}
	if want := bytes.Repeat([]byte{11}, 32); !bytes.Equal(key, want) {
		t.Fatal("tenant-prefixed explicit key was not used")
	}
}

func TestEnvelopeRoundTripAndAAD(t *testing.T) {
	p := newTestProvider(t)
	sealed, err := p.Seal([]byte("secret body"), "acme", "diva-shared", "chunks/logs/partition=diva-shared/segment.parquet")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEnvelope(sealed) {
		t.Fatal("sealed value is not an envelope")
	}
	if bytes.Contains(sealed, []byte("secret body")) {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := p.Open(sealed, "chunks/logs/partition=diva-shared/segment.parquet")
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "secret body" {
		t.Fatalf("plaintext = %q", plain)
	}
	if _, err := p.Open(sealed, "chunks/logs/partition=divb-shared/segment.parquet"); err == nil {
		t.Fatal("AAD mismatch must fail")
	}
}

func TestObserveAllowsPlaintextEnforceRejects(t *testing.T) {
	p := newTestProvider(t)
	got, err := p.Open([]byte("legacy plaintext"), "path")
	if err != nil || string(got) != "legacy plaintext" {
		t.Fatalf("observe plaintext = %q, %v", got, err)
	}

	cfg := testConfig(t)
	cfg.Mode = ModeEnforce
	strict, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Open([]byte("legacy plaintext"), "path"); err == nil {
		t.Fatal("enforce mode must reject plaintext")
	}
}

func TestFieldRoundTrip(t *testing.T) {
	p := newTestProvider(t)
	value, err := p.EncryptField("payment declined", "acme", "diva-shared", "logs.raw_message")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncryptedField(value) {
		t.Fatalf("field value %q is not marked encrypted", value)
	}
	if strings.Contains(value, "payment declined") {
		t.Fatal("field contains plaintext")
	}
	plain, err := p.DecryptField(value, "logs.raw_message")
	if err != nil {
		t.Fatal(err)
	}
	if plain != "payment declined" {
		t.Fatalf("plaintext = %q", plain)
	}
	if _, err := p.DecryptField(value, "traces.name"); err == nil {
		t.Fatal("field name is part of the AAD and must not be interchangeable")
	}
}

func TestWrongKeyFails(t *testing.T) {
	cfg := testConfig(t)
	cfg.RootKey = bytes.Repeat([]byte{2}, 32)
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := p.Seal([]byte("secret"), "acme", "diva-shared", "path")
	if err != nil {
		t.Fatal(err)
	}

	cfg.RootKey = bytes.Repeat([]byte{3}, 32)
	wrong, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Open(sealed, "path"); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestParseKeyFileRejectsShortKey(t *testing.T) {
	_, err := ParseKeyFile([]byte(`{"version":1,"root_key":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`))
	if err == nil {
		t.Fatal("short root key must fail")
	}
}

func TestLoadProviderPrefersJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"version":     1,
		"tenant":      "acme",
		"root_key_id": "root-v1",
		"root_key":    testKey(t, 4),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadProvider(Config{Enabled: true, KeysFile: "/does/not/exist", KeysJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.KeyFor("acme", "diva-shared"); err != nil {
		t.Fatal(err)
	}
}

func TestParseSetting(t *testing.T) {
	cases := map[string]struct {
		want Setting
		ok   bool
	}{
		"":            {Setting{}, true},
		"off":         {Setting{}, true},
		"disabled":    {Setting{}, true},
		"read":        {Setting{Read: true}, true},
		"decrypt":     {Setting{Read: true}, true},
		"envelope":    {Setting{Write: true, Read: true}, true},
		"on":          {Setting{Write: true, Read: true}, true},
		"not-a-valid": {Setting{}, false},
	}
	for input, tc := range cases {
		got, err := ParseSetting(input)
		if tc.ok && err != nil {
			t.Fatalf("ParseSetting(%q) error = %v", input, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("ParseSetting(%q) expected an error", input)
		}
		if got != tc.want {
			t.Fatalf("ParseSetting(%q) = %+v, want %+v", input, got, tc.want)
		}
	}
}

func TestDecryptOnlyProviderReadsEnvelopeButWritesPlaintext(t *testing.T) {
	raw := []byte(`{
		"version": 1,
		"tenant": "default",
		"root_key_id": "root-v1",
		"root_key": "` + testKey(t, 5) + `"
	}`)
	writerCfg, err := ParseKeyFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	writerCfg.Enabled = true
	writer, err := NewProvider(writerCfg)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := writer.Seal([]byte("historical secret"), "default", "guardian-configs", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEnvelope(sealed) {
		t.Fatal("writer did not produce an envelope")
	}

	readerCfg, err := ParseKeyFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	readerCfg.Enabled = false
	readerCfg.DecryptOnly = true
	reader, err := NewProvider(readerCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Active() || reader.Enabled() || !reader.DecryptOnly() {
		t.Fatalf("decrypt-only provider state = active:%v enabled:%v decryptOnly:%v", reader.Active(), reader.Enabled(), reader.DecryptOnly())
	}
	got, err := reader.Open(sealed, "ctx")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if string(got) != "historical secret" {
		t.Fatalf("Open() = %q", got)
	}

	plain, err := reader.Seal([]byte("new plaintext"), "default", "guardian-configs", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if IsEnvelope(plain) {
		t.Fatal("decrypt-only provider must not encrypt new writes")
	}
	if string(plain) != "new plaintext" {
		t.Fatalf("Seal() = %q", plain)
	}
}

func TestDecryptOnlyRequiresKeys(t *testing.T) {
	_, err := NewProvider(Config{DecryptOnly: true})
	if err == nil {
		t.Fatal("expected decrypt-only without keys to fail")
	}
}

func TestDecryptOnlyEnforceRejectsPlaintext(t *testing.T) {
	cfg := testConfig(t)
	cfg.Enabled = false
	cfg.DecryptOnly = true
	cfg.Mode = ModeEnforce
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open([]byte("legacy plaintext"), "ctx"); err == nil {
		t.Fatal("expected enforce mode to reject plaintext")
	}
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := NewProvider(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testConfig(t *testing.T) Config {
	t.Helper()
	key, err := decodeKey(testKey(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Enabled:   true,
		Mode:      ModeObserve,
		Tenant:    "acme",
		RootKeyID: "root-v1",
		RootKey:   key,
	}
}
