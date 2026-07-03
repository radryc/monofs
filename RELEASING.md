# Releasing MonoFS

## Versioning

MonoFS follows [Semantic Versioning](https://semver.org/): `MAJOR.MINOR.PATCH`.

- **MAJOR**: breaking proto or on-disk format changes.
- **MINOR**: new gRPC endpoints, CLI flags, or config top-level keys.
- **PATCH**: bug fixes, performance improvements, doc changes.

Version is injected at build time via linker flags:

```
go build -ldflags "-X main.Version=1.2.3 -X main.Commit=$(git rev-parse HEAD) -X main.BuildTime=$(date -u +%FT%TZ)"
```

## Release process

1. **Create a release branch** from `main`:
   ```
   git checkout -b release/v1.2.3 main
   ```

2. **Update CHANGELOG.md** with the release date and version header.

3. **Tag the release**:
   ```
   git tag -a v1.2.3 -m "Release v1.2.3"
   git push origin v1.2.3
   ```

4. **Build artifacts**:
   ```
   make build
   ```
   Binaries land in `bin/`.

5. **Build Docker images**:
   ```
   docker build -t monofs:1.2.3 .
   docker build -t monofs-search:1.2.3 -f Dockerfile.search .
   ```
   Or use the Docker Compose setups:
   ```
   docker compose -f docker-compose.yml build
   ```

6. **Verify** using the load test tool:
   ```
   ./bin/monofs-loadtest
   ```

## Rollback

To roll back to a previous release:

1. Deploy the previous version's images.
2. Apply the previous version's Guardian configuration (bootstrap maps).
3. Drain the current router and start the previous version.
4. Verify cluster health via `monofs-admin status`.

State directories (`--state-dir`, `--workspace-state-dir`) are forward-compatible within the same MAJOR version. Ensure a backup exists before rolling back across MAJOR versions.

## Release checklist

- [ ] All CI workflows pass on the release branch
- [ ] Proto files are up to date (`make proto` produces no diff)
- [ ] Race detector clean (`make test-race`)
- [ ] CHANGELOG.md updated with release date
- [ ] Tag pushed
- [ ] Docker images built and pushed to registry
- [ ] Load test passes against a staging cluster
- [ ] Release notes published in GitHub releases
