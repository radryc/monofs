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

// GrantEvaluator decides whether an identity may perform an action on a
// partition. Implementations resolve grants from the runtime policy store,
// OWNERS files, and IdP group membership.
type GrantEvaluator interface {
	// Can reports whether the identity is permitted to perform action on the
	// named partition.
	Can(ctx context.Context, id Identity, partition string, action Action) bool
}

// NoopVerifier is a TokenVerifier that treats every token as an anonymous
// identity without error. It is used during OBSERVE-mode rollout before real
// OIDC verification is enabled.
type NoopVerifier struct{}

// Verify always returns an anonymous identity and no error.
func (NoopVerifier) Verify(context.Context, string) (Identity, error) {
	return Identity{}, nil
}

// AllowAllEvaluator permits every action. It is the OBSERVE-mode default so
// enforcement can be layered in without changing behavior.
type AllowAllEvaluator struct{}

// Can always returns true.
func (AllowAllEvaluator) Can(context.Context, Identity, string, Action) bool {
	return true
}

// DenyAllEvaluator rejects every action. Useful as a safe default in tests.
type DenyAllEvaluator struct{}

// Can always returns false.
func (DenyAllEvaluator) Can(context.Context, Identity, string, Action) bool {
	return false
}
