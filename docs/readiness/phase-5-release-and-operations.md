# Phase 5 — Release and Operational Completeness

## Design

### Objective
Close release-process and documentation gaps so production rollout and rollback are deterministic and audited.

### Current baseline

- Missing release docs at repo root (`RELEASING.md`, `CHANGELOG.md`) and no formalized release automation.
- `cmd/monofs-admin/README.md` links currently point to missing docs:
  - `../DEPLOYMENT.md`
  - `../k8s/README.md`
  - `../k8s/PRODUCTION.md`
- Session docs already describe local virtual commits plus separate push flow, but phase work should lock this language to implementation changes in Phase 2/3.

### Target release model

- Introduce explicit versioning and rollout policy:
  - semantic versioning rules
  - release branches/tags
  - rollback paths
  - artifact matrix (binaries + images)
- Changelog generation policy tied to commit metadata/labels.

### Operational docs model

- Add missing deployment docs and ensure all internal links resolve.
- Align user-facing CLI docs with actual behavior:
  - local virtual commit creation vs upstream push
  - branch strategy and policy gating semantics
  - preserve vs squash mode behavior.

## Implementation plan

1. Add root release docs:
   - `RELEASING.md`
   - `CHANGELOG.md`
2. Add deployment docs currently referenced by admin README:
   - `DEPLOYMENT.md`
   - `k8s/README.md`
   - `k8s/PRODUCTION.md`
3. Update `cmd/monofs-admin/README.md` and `docs/README.md` links to canonical doc locations.
4. Update `docs/usage.md` and `cmd/monofs-session/README.md` terminology and examples to exactly match implemented commit/push semantics.
5. Add release workflow automation document and script targets aligned with existing Make/Guardian/StrataTools operational flow.
6. Add a link-check step in CI so dead internal links are blocked in PRs.

