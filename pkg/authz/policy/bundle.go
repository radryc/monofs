package policy

import (
	"encoding/json"
	"fmt"
)

// FormatVersion is the compiled bundle format version.
const FormatVersion = 1

// ParseBundle parses a compiled policy bundle.
func ParseBundle(data []byte) (*Policy, error) {
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("policy: parse bundle: %w", err)
	}
	if bundle.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("policy: unsupported bundle format %d (expected %d)", bundle.FormatVersion, FormatVersion)
	}
	return New(bundle), nil
}

// MarshalBundle renders a bundle as indented JSON.
func MarshalBundle(bundle Bundle) ([]byte, error) {
	if bundle.FormatVersion == 0 {
		bundle.FormatVersion = FormatVersion
	}
	if bundle.Version == "" {
		bundle.Version = computeVersion(&bundle)
	}
	return json.MarshalIndent(bundle, "", "  ")
}

// MarshalPolicy renders a policy's bundle as indented JSON.
func MarshalPolicy(p *Policy) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("policy: no policy")
	}
	return MarshalBundle(p.Bundle())
}
