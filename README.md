Kube Risc Agent (Hackathon MVP)

Overview

- Watches only NodeAction for the local node via `fieldSelector=spec.nodeName=$(NODE_NAME)`.
- Resolves InstructionSet. For all runtimes (including `exec`), the agent calls your external RuntimeDriver gRPC service. For `runtime=exec`, it renders `execTemplate.command` (Go text/template) with `.SubjectID`, `.Params.*`, `.Node.*`, `.Annotations.*`, then passes the rendered argv to the driver in `params["_argv"]` as a JSON array string. The driver performs the actual execution.
- Writes back `status.phase/conditions/result{stdoutTail,stderrTail,artifacts}` and emits Events.
- Concurrency limited with semaphore (default 4) and Prometheus metrics exposed on `:8080/metrics`.

Layout

- `cmd/agent/main.go` — entrypoint, env/flags, metrics server
- `internal/agent/agent.go` — core watch loop and state machine
- `internal/template/render.go` — strict template renderer (missing keys error)
- `internal/util/ringbuffer.go` — last-N-bytes buffers for stdout/stderr tails
- `deploy/` — DaemonSet and RBAC manifests (adjust CRD groups as needed)
- `examples/` — sample InstructionSet and NodeAction

Firecracker Example (firectl)

- A minimal Firecracker InstructionSet is provided: `examples/instructionset-firecracker.yaml` (runtime `exec`). It defines a `run` instruction using `/usr/local/bin/firectl` with `kernel/rootfs/vcpu/memMiB` params, and derives the API socket path from `.SubjectID`.
- Try it (replace `YOUR_NODE_NAME`):
  - `kubectl apply -f examples/instructionset-firecracker.yaml`
  - `kubectl apply -f examples/nodeaction-firecracker-run.yaml`
  - `kubectl get nodeaction -A` and watch `status.phase`/`result.*`
- Driver note: ensure the Runtime Driver allows and executes the rendered argv (add `/usr/local/bin/firectl` to its binary allowlist, or route `_argv`). For pause/resume/snapshot/restore you typically need to call Firecracker's API (e.g., via curl) — extend your InstructionSet/Driver as needed.

Wasmtime Example

- A minimal Wasmtime InstructionSet is provided: `examples/instructionset-wasmtime.yaml` (runtime `exec`). It defines a `run` instruction using `/usr/bin/wasmtime` and requires a single param `module` (absolute path to the `.wasm` inside the Driver pod).
- ActionRequest sample to run a hello world: `examples/actionrequest-wasmtime-hello.yaml`. Label a pod in the same namespace with `app=demo` and ensure the module is mounted at `/opt/wasm/hello.wasm` in the Driver container.
- Driver note: add `/usr/bin/wasmtime` to the Driver binary allowlist. The agent renders argv and sends it via `params["_argv"]`.

Wasmtime over OCI (Driver pulls module)

- InstructionSet (gRPC runtime): `examples/instructionset-wasmtime-oci.yaml`
  - Params: `moduleRef` (oci://...), `moduleDigest` (sha256:...), optional `args`.
  - Agent will NOT render argv for `runtime=grpc`; it passes params to the Runtime Driver.
- ActionRequest sample: `examples/actionrequest-wasmtime-oci-hello.yaml`
- Driver requirements:
  - Implement fetching `moduleRef` from an OCI registry, verify `moduleDigest`, cache locally (e.g., `/var/lib/kuberisc/modules/<sha256>.wasm`).
  - Execute via `/usr/bin/wasmtime <cached_path> [args...]` and return stdout/stderr tail.
  - Restrict allowed registries and enable auth if needed.

Configuration

- `NODE_NAME` (required): set via DownwardAPI.
- `AGENT_CONCURRENCY` (default 4): internal parallelism.
- `NA_GROUP`/`NA_VERSION`/`NA_RESOURCE`: NodeAction GVR (defaults: `risc.dev/v1alpha1`, `nodeactions`).
- `IS_GROUP`/`IS_VERSION`/`IS_RESOURCE`: InstructionSet GVR (defaults to `risc.dev/v1alpha1`, `instructionsets`).
- `METRICS_ADDR` (default `:8080`): metrics endpoint.
- `DRIVER_ADDR` (required): gRPC driver address. Examples: `unix:///var/run/runtime-driver.sock` or `driver-svc:50051`.
- `DRIVER_INSECURE` (default `true`): use insecure transport to the driver (set to `false` for TLS).
- `NA_NODE_LABEL` (optional): label key used for server-side filtering via labelSelector. If set (e.g. `risc.dev/node`), the agent lists/watches NodeActions with `labelSelector="<key>=<NODE_NAME>"`. Ensure your NodeAction objects include `metadata.labels[<key>]=<NODE_NAME>`.

CRD Expectations (MVP)

- NodeAction.spec fields used: `nodeName`, `instructionRef{runtime,name,instruction}`, `resolvedSubjectID`, `params`, `timeoutSeconds`, `executionID`.
- NodeAction.status fields written: `phase` (Pending→Running→Succeeded|Failed), `startedAt`, `finishedAt`, `conditions[Executed]`, `result.stdoutTail`, `result.stderrTail`, `result.artifacts`.
- InstructionSet layout:
  - For `runtime=exec`: `spec.execTemplates.<runtime>.<instruction>.command: []string` used for argv template rendering.
  - For `runtime=grpc`: `execTemplate` is not required; only `paramsSchema.properties` is used for param whitelist.
  - Optional `spec.paramsSchema.properties` for param whitelist (keys only).

Security & RBAC

- Param whitelist: only keys declared under `spec.paramsSchema.properties` are passed to templates.
- Secret injection (optional): `valueFrom.secretKeyRef` is supported; values are written to temp files and `.Params.<key>` becomes the file path. Files are removed after execution.
- RBAC: see `deploy/rbac.yaml`. Adjust API groups and resource names to match your CRDs.

Deploy

1. Build and push the image for the agent and update `deploy/daemonset.yaml` image.
2. Apply RBAC: `kubectl apply -f deploy/rbac.yaml`
3. Deploy DaemonSet: `kubectl apply -f deploy/daemonset.yaml`
4. Expose metrics for Prometheus scraping: `kubectl apply -f deploy/metrics-service.yaml`
5. If Prometheus Operator is installed, create a ServiceMonitor: `kubectl apply -f deploy/servicemonitor.yaml`
4. Create an InstructionSet and a NodeAction (examples in `examples/`). Ensure `spec.nodeName` matches the node.
   - If you set `NA_NODE_LABEL`, also add the label to your NodeAction, for example: `metadata.labels["risc.dev/node"]=<nodeName>`.

gRPC Runtime Driver

- Proto: service `runtime.v1.RuntimeDriver` with `Execute(ExecuteRequest) returns (ExecuteReply)`. The Go package path is `github.com/WangQiHao-Charlie/driver/api/proto/runtime/v1` (import as `runtimev1`).
- Agent behavior for all runtimes (including `exec`):
  - Passes `instructionRef.instruction` → `ExecuteRequest.instruction`.
  - Passes `spec.resolvedSubjectID` → `subject_id`.
  - Passes filtered params (map[string]string) → `params` (non-strings JSON-encoded).
  - For `runtime=exec`, additionally passes `params["_argv"]` containing the rendered argv as a JSON array string (driver should interpret and execute this argv).
  - Passes `spec.executionID` → `execution_id` for idempotence.
  - Uses NodeAction `timeoutSeconds` to bound the RPC; on deadline, marks `Timeout` and fails the action.
  - Writes driver `exit_code/stdout_tail/stderr_tail/artifacts` to status.

The driver repository is public: `https://github.com/WangQiHao-Charlie/driver`, no special `GOPRIVATE` or token needed.

Notes

- The agent no longer executes commands locally. For `runtime=exec`, argv is rendered and handed off to the gRPC driver via `_argv`.
- Idempotence: the agent only claims `Pending` actions by transitioning to `Running`. Retries are controlled by the controller via new NodeActions.

Grafana (optional)

- A ready-to-use Grafana stack is provided under `../grafana/` with:
  - Prometheus datasource (expects `prometheus-k8s.monitoring.svc:9090`, as in kube-prometheus-stack)
  - A "KubeRISC NodeActions" dashboard (success rate, duration quantiles, active actions)
- Apply Grafana: `kubectl apply -k ../grafana`
- Access: `kubectl -n monitoring port-forward svc/grafana 3000:3000` then open http://localhost:3000 (anonymous viewer enabled; admin password `admin`).

Publish to GHCR

- This repo includes a workflow `.github/workflows/publish.yml` that builds multi-arch images (amd64/arm64) and pushes to GHCR using the built-in `GITHUB_TOKEN`.
- Image name: `ghcr.io/<OWNER>/kuberisc-agent` (owner is your GitHub org/user). Update `deploy/daemonset.yaml` accordingly.
- Triggers:
  - Push a tag like `v0.1.0` → publishes tag and `sha` tag.
  - Push to `main/master` → publishes `latest` and `sha` tag.
- Manual run: Go to Actions → Publish Image → Run workflow.
- Dockerfile is in repo root; distroless base, static binary.
