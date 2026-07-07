# Foo Application End-to-End Examples

## Purpose

This guide shows two complete examples for a simple Go service named `foo`:

- local development with a locally managed SRE Tool Hub environment
- production-oriented development against a real cluster where MonoFS is already deployed

The goal is not to prescribe one exact repo layout. The goal is to show how MonoFS fits the full cycle of build, develop, deploy, and monitor.

## The Example Application

Assume `foo` is a minimal Go HTTP service.

Example `main.go`:

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    "os"
)

func main() {
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }

    http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("ok"))
    })

    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        _, _ = fmt.Fprintf(w, "foo says hello\n")
    })

    log.Fatal(http.ListenAndServe(":"+port, nil))
}
```

Example `Dockerfile`:

```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/foo ./...

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/foo /usr/local/bin/foo
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/foo"]
```

Assume the upstream repo already exists at:

```text
https://gitlab.com/your-org/foo.git
```

and you want it to appear inside MonoFS as:

```text
sre/foo
```

## Example 1: Local Development Environment

This is the full loop when you manage the platform locally.

### Phase 1: Bring up the platform

```bash
guardianctl dev setup
guardianctl dev init
```

This gives you local MonoFS, Guardian.

### Phase 2: Ingest and mount `foo`

From `monofs`:

```bash
./bin/monofs-admin ingest \
  --router localhost:9090 \
  --source https://gitlab.com/your-org/foo.git \
  --source-id sre/foo

./bin/monofs-client \
  --mount /tmp/monofs \
  --router localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay /tmp/monofs-overlay
```

At this point, the repo appears in the mounted workspace at `sre/foo`.

### Phase 3: Develop inside the mounted workspace

Open the workspace and edit the service:

```bash
cd /tmp/monofs/sre/foo
go test ./...
go build ./...
```

Make your code change, then inspect it through MonoFS:

```bash
cd /home/rydzu/strata/monofs
./bin/monofs-session status
./bin/monofs-session diff
```

### Phase 4: Build and deploy locally

The application repo can build its own image with normal tooling. The deployment side belongs in Guardian-managed config.

A practical pattern is:

1. keep source in `sre/foo`
2. keep deployment intent in a Guardian partition such as `sre/foo/partrtition/dev` or another environment-specific partition
3. build the image from the source repo
4. update the Guardian-managed deployment manifest to point at the new image
5. reconcile the partition

Concrete sample Guardian intent YAML for `foo`:

```yaml
apiVersion: guardian.sretoolhub/v1alpha1
kind: Intent
metadata:
  name: foo
  partition: dev-workspace
spec:
  deploy:
    driver: kubernetes
    objects:
      - apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: foo
          namespace: foo
          env: dev
          labels:
            app: foo
            env: dev
        spec:
          replicas: 1
          selector:
            matchLabels:
              app: foo
          template:
            metadata:
              labels:
                app: foo
            spec:
              containers:
                - name: foo
                  image: registry.sretoolhub.local/foo:dev
                  imagePullPolicy: IfNotPresent
                  ports:
                    - containerPort: 8080
                  env:
                    - name: PORT
                      value: "8080"
                  readinessProbe:
                    httpGet:
                      path: /healthz
                      port: 8080
                    initialDelaySeconds: 3
                    periodSeconds: 5
                  livenessProbe:
                    httpGet:
                      path: /healthz
                      port: 8080
                    initialDelaySeconds: 10
                    periodSeconds: 10
      - apiVersion: v1
        kind: Service
        metadata:
          name: foo
          namespace: foo
        spec:
          selector:
            app: foo
          ports:
            - name: http
              port: 80
              targetPort: 8080
```

In a mounted workspace this would typically live at a path such as:

```text
sre/foo/partititon/dev/intents/foo.yaml
```

Representative local image build step:

```bash
docker build -t foo:dev /tmp/monofs/sre/foo
```

Representative Guardian-managed deployment flow:

```bash
# edit the partition YAML under the mounted guardian namespace
# for example sre/foo/parittion/dev/intents/foo.yaml

./bin/monofs-session status
./bin/monofs-session diff
./bin/monofs-session commit -m "Update foo image and config"
```

If your environment uses partition release flows from `stratatools`, reconcile the relevant partition after updating the intent.

### Phase 5: Monitor locally

After rollout, validate the service using both application and platform signals.

Suggested checks:

- `curl http://<local-foo-endpoint>/healthz`
- MonoFS health dashboards from the monitoring partition
- Guardian rollout dashboards for intent convergence
- Doctor dashboards for logs, metrics, and traces

What MonoFS adds here is not the HTTP probe itself. It is the shared workspace where source, rollout intent, and operational context live side by side.

### Phase 6: Publish and refresh

Once satisfied, publish the source changes back upstream:

```bash
./bin/monofs-session commit -m "Implement foo health endpoint"
./bin/monofs-session pull
```

This completes the local full cycle:

- build platform locally
- ingest repo
- mount workspace
- develop in MonoFS
- update Guardian-managed deployment state
- validate through Doctor and monitoring
- publish upstream

## Example 2: Production Cluster Already Running MonoFS

This is the same service lifecycle when the cluster already exists and you are connecting a local workstation to it.

### Phase 1: Connect to the production MonoFS router

Either use the production router endpoint directly or create a controlled local tunnel:

```bash
kubectl port-forward -n monofs svc/monofs-router 9090:9090 8080:8080
```

Then verify health:

```bash
./bin/monofs-admin status --router <url>:9090
```

### Phase 2: Ensure `foo` is available in the cluster

If the repo is not already ingested:

```bash
./bin/monofs-admin ingest \
  --router <url>:9090 \
  --source https://gitlab.com/your-org/foo.git \
  --source-id sre/foo
```

If it is already present, you can go straight to the existing mounted workspace.

### Phase 3: Use the existing production-mounted workspace

Assume the production environment already provides a mounted MonoFS workspace.

For example, `foo` may already be available at a path such as `/mnt/monofs/sre/foo` or another environment-managed workspace root.

### Phase 4: Make the production change

Edit the service and validate it locally first:

```bash
cd /mnt/monofs/sre/foo
go test ./...
go build ./...
```

Inspect the pending workspace changes:

```bash
cd /home/rydzu/strata/monofs
./bin/monofs-session status
./bin/monofs-session diff
```

### Phase 5: Update deployment intent

If production rollout is Guardian-managed, update the relevant partition intent visible in the same workspace under `guardian/<partition>/...`.

Typical pattern:

1. update the image reference or config values for `foo`
2. ensure the deployment intent matches the new release
3. publish the workspace changes with a clear commit message

Representative production intent change:

```yaml
apiVersion: guardian.sretoolhub/v1alpha1
kind: Intent
metadata:
  name: foo
  partition: production
spec:
  deploy:
    driver: kubernetes
    objects:
      - apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: foo
          namespace: foo
        spec:
          replicas: 3
          template:
            spec:
              containers:
                - name: foo
                  image: registry.sretoolhub.example/foo:2026-07-06-1
                  ports:
                    - containerPort: 8080
                  readinessProbe:
                    httpGet:
                      path: /healthz
                      port: 8080
```

```bash
./bin/monofs-session commit -m "Roll foo production fix"
```

### Phase 6: Observe the rollout

In production, monitoring is the decisive phase.

Recommended checks:

- cluster and repository health with `monofs-admin status`
- Guardian rollout dashboards for reconciliation status
- Doctor dashboards for errors, latency, ingest health, and traces
- application-specific dashboards and alerts from the monitoring partition

Representative application checks:

```bash
curl https://foo.your-domain.example/healthz
```

Representative platform checks:

- verify no new MonoFS cluster health regressions
- confirm Guardian reports the target partition converged
- confirm Doctor shows healthy logs, metrics, and traces for `foo`

### Phase 7: Refresh the workspace

After the remote state advances, refresh the mounted workspace:

```bash
./bin/monofs-session pull
```

This completes the production-oriented full cycle:

- connect to the running cluster
- use the existing mounted workspace backed by the real deployment environment
- change source and rollout intent from one place
- publish deliberately
- monitor the result using Guardian and Doctor signals
- refresh the workspace to the new upstream truth

## Key Takeaways

The `foo` examples show the main value of MonoFS:

- source code can stay in its own repo
- deployment intent can stay Guardian-managed
- telemetry can stay Doctor-backed
- the user still gets one workspace for the whole loop

That is the MonoFS pitch in practical form.

For incident and rollout runbooks, see [Production Operations Playbook](production-operations.md).
