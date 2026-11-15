package authz

import "context"

// TokenVerifier turns an opaque/bearer token into a verified Identity.
// Implementations validate signatures, expiry, audience and issuer.
type TokenVerifier interface {
	// Verify validates the raw token and returns the resulting Identity.
	// It returns an error when the token is missing, malformed, expired, or
	// otherwise untrusted.
	Verify(ctx context.Context, rawToken string) (Identity, error)
}

// NoopVerifier is a TokenVerifier that treats every token as an anonymous
// identity without error. It is used during OBSERVE-mode rollout before real
// OIDC verification is enabled.
type NoopVerifier struct{}

// Verify always returns an anonymous identity and no error.
func (NoopVerifier) Verify(context.Context, string) (Identity, error) {
	return Identity{}, nil
}
