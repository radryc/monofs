package search

import (
	"context"
	"path"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/monopath"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InternalClientID is the machine identity attached by the search service
// interceptor to calls that present the cluster-internal token.
const InternalClientID = "monofs-internal"

// internalCallKey marks requests that arrived with the cluster-internal token.
// It is deliberately private so only ContextWithInternalCall can set it and a
// verified bearer identity cannot impersonate an internal call.
type internalCallKey struct{}

// ContextWithInternalCall returns a copy of ctx marked as a cluster-internal
// call. Internal calls bypass access-policy filtering.
func ContextWithInternalCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalCallKey{}, true)
}

// IsInternalCall reports whether ctx carries the internal-call marker.
func IsInternalCall(ctx context.Context) bool {
	internal, _ := ctx.Value(internalCallKey{}).(bool)
	return internal
}

// hitDisplayPath resolves the repository display path of a search hit. Hits
// carry the repository display path plus a repo-relative file path; joining
// them and re-splitting keeps the mapping consistent with monopath
// (e.g. guardian/<partition> repository roots).
func hitDisplayPath(hit *pb.SearchResult) string {
	if hit == nil {
		return ""
	}
	full := hit.DisplayPath
	if hit.FilePath != "" {
		full = path.Join(hit.DisplayPath, hit.FilePath)
	}
	if displayPath, _, ok := monopath.SplitDisplayPath(full); ok {
		return displayPath
	}
	// Fall back to the repository root carried on the hit.
	return strings.Trim(strings.TrimSpace(hit.DisplayPath), "/")
}

// filterHits keeps only the hits whose repository display path the identity may
// read. It returns the kept hits and the number of dropped hits. A nil policy
// keeps every hit. Admin identities bypass per-path grants via
// Policy.CanReadPath.
func filterHits(p *policy.Policy, id authz.Identity, hits []*pb.SearchResult) ([]*pb.SearchResult, int) {
	if p == nil {
		return hits, 0
	}
	admin := p.IsAdmin(id)
	kept := make([]*pb.SearchResult, 0, len(hits))
	dropped := 0
	for _, hit := range hits {
		if hit == nil {
			continue
		}
		if admin || p.CanReadPath(id, hitDisplayPath(hit)) {
			kept = append(kept, hit)
			continue
		}
		dropped++
	}
	return kept, dropped
}

// applyReadPolicy applies the configured access policy to search hits. It
// returns the hits to expose, the number of hits the policy denied, and an
// error when enforcement rejects the request.
//
// When no policy is configured or the call is cluster-internal the hits are
// returned unchanged. In enforce mode anonymous callers and hits outside the
// caller's grants are rejected/dropped; in observe mode (the rollout default)
// the same decisions are logged but every hit is returned.
func (s *Service) applyReadPolicy(ctx context.Context, hits []*pb.SearchResult) ([]*pb.SearchResult, int, error) {
	if s.policy == nil || IsInternalCall(ctx) {
		return hits, 0, nil
	}

	p := s.policy.Get()
	if p == nil {
		// No policy fetched yet: fail closed when enforcing, otherwise keep
		// the previous (unfiltered) behavior.
		if s.enforceRead {
			return nil, 0, status.Error(codes.PermissionDenied, "access policy unavailable")
		}
		s.logger.Warn("search: access policy not loaded; returning unfiltered results (observe)")
		return hits, 0, nil
	}

	id, _ := authz.IdentityFromContext(ctx)
	if id.IsAnonymous() {
		if s.enforceRead {
			return nil, 0, status.Error(codes.PermissionDenied, "authentication required")
		}
		s.logger.Info("search: anonymous caller; returning unfiltered results (observe)",
			"hits", len(hits),
			"policy_version", p.Version())
		return hits, 0, nil
	}

	kept, dropped := filterHits(p, id, hits)
	if dropped > 0 {
		s.logger.Info("search: access policy decision",
			"principal", id.PrincipalID(),
			"policy_version", p.Version(),
			"enforce", s.enforceRead,
			"hits", len(hits),
			"kept", len(kept),
			"filtered", dropped)
	} else {
		s.logger.Debug("search: access policy allowed all hits",
			"principal", id.PrincipalID(),
			"policy_version", p.Version(),
			"hits", len(hits))
	}
	if !s.enforceRead {
		// Observe mode: log the decision but keep every hit.
		return hits, dropped, nil
	}
	return kept, dropped, nil
}
