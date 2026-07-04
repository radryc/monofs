# Phase 4 — Repository Governance, CI, and Deployment Pipelines

## Design

### Objective
Introduce repository governance and production-grade CI/CD gates with deployment controls.

### Current baseline

- No `.github/` governance or workflow files in this repository.
- Existing deployment flows are Make + Docker Compose (`Makefile`, `docker-compose*.yml`) and Strata bootstrap/release orchestration through Guardian/StrataTools:
  - `~/strata/guardian/Makefile`
  - `~/strata/stratatools/docs/USAGE.md`
  - `~/strata/stratatools/deploy/bootstrap/bootstrap.yaml`

### Governance baseline

- Add:
  - `.github/CODEOWNERS`
  - `.github/pull_request_template.md`
  - `.github/ISSUE_TEMPLATE/*`
- Define required checks to match branch protection policy.

### CI design

- Workflows:
  - `ci-go.yml`: build/unit/vet/gofmt checks
  - `ci-proto.yml`: generated proto consistency
  - `security.yml`: govulncheck + dependency scan
  - `docker-build.yml`: build impacted Docker targets
- Use path filters and matrix minimization to control cost.

### Deployment design

- Add staged deployment workflows that align with existing Strata/Guardian runtime:
  - staging deploy
  - production deploy with manual approvals
- Reuse existing bootstrap/release entrypoints rather than inventing parallel deployment tooling:
  - `guardianctl dev/bootstrap/release` flows
  - stratatools partition release/stamp mechanics.

## Implementation plan

1. Create `.github` governance assets and owners map by subsystem:
   - router, fetcher, fuse, server, docs, CI.
2. Add CI workflows with explicit required-status naming.
3. Ensure CI uses the same commands already used by repo maintainers (Make/go test/proto checks).
4. Add deployment workflows:
   - non-prod: automated on main
   - prod: manual dispatch + environment protection + reviewer approvals.
5. Integrate deployment commands with existing Guardian/StrataTools flow:
   - bootstrap/update via `guardianctl`
   - partition release via `guardianctl release run` / stratatools image flow.
6. Document branch protection check list and required workflow names in repo docs.

