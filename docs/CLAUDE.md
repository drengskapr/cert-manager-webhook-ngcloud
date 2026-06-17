# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A cert-manager ACME DNS-01 webhook solver for the [Nubes](https://ngcloud.ru) cloud platform (ngcloud.ru). The webhook implements the cert-manager external webhook solver interface to automate TLS certificate issuance by creating/deleting DNS TXT records via the Nubes deck-api.

## Repository Structure

```
cert-manager-webhook-nubes/
├── main.go                  — entry point; reads GROUP_NAME env var, initializes logger, calls cmd.RunWebhookServer
├── solver.go                — NgcloudSolver struct, NgcloudSolverConfig, Present/CleanUp/Initialize (with klog logging)
├── solver_test.go           — cert-manager conformance test (requires TEST_ZONE_NAME + TEST_ZONE_UID env vars)
├── go.mod / go.sum          — module: cert-manager-webhook-ngcloud, Go 1.25.6
├── Dockerfile               — multi-stage build (golang:1.25.6 + distroless/static)
├── Makefile                 — build / test / docker-build / helm-install targets
├── .gitignore               — excludes testdata/ngcloud/secret.yaml and the webhook binary
├── ngcloud/
│   └── client.go            — deck-api HTTP client (CreateTXTRecord / DeleteTXTRecord)
├── testdata/
│   └── ngcloud/
│       └── secret.yaml      — K8s Secret manifest loaded by conformance test (gitignored; fill in real token before testing)
├── docs/
│   ├── CLAUDE.md            — this file
│   └── PLAN.md              — implementation plan with completion status
└── deploy/
    └── cert-manager-webhook-ngcloud/   — Helm chart
        ├── Chart.yaml
        ├── values.yaml
        └── templates/
            ├── _helpers.tpl
            ├── deployment.yaml
            ├── service.yaml
            ├── apiservice.yaml
            ├── rbac.yaml
            └── tls.yaml
```

## Code Conventions

- All code, comments, and identifiers must be in English.
- The deck-api returns CFS parameter labels in Russian. These are encoded as Unicode escape
  sequences in named English constants in `ngcloud/client.go` (`cfsLabelZoneUID`, etc.) so
  that no Cyrillic characters appear in source code.

## Logging

Structured logging via `k8s.io/klog/v2` in `solver.go`. The controller-runtime logger is
initialized in `main.go` using `go.uber.org/zap` with `zapcore.ISO8601TimeEncoder` (all
timestamps in ISO 8601 format). Logs go to stdout and are suitable for any log aggregator.

## Dependencies

Key versions (see `go.mod`):
- `github.com/cert-manager/cert-manager v1.19.4`
- `k8s.io/{api,apimachinery,client-go} v0.34.1`
- `sigs.k8s.io/controller-runtime v0.22.3`

All k8s indirect deps (apiserver, apiextensions-apiserver, component-base, kms) are pinned
to v0.34.1 to match cert-manager v1.19.4.

## Nubes deck-api

Base URL: `https://lk-api-gateway.ngcloud.ru/api/v1/svc`. Auth: Bearer token via `Authorization` header.

> **Endpoint migration (2026-06-15):** the client now targets the `lk-api-gateway.ngcloud.ru/api/v1/svc`
> gateway. The previous base URL was `https://deck-api.ngcloud.ru/api/v1/index.cfm`. The base URL is an
> unexported default in `ngcloud/client.go` and is not user-configurable, so this is an internal change
> (no `Issuer`, Helm values, or Secret changes required).

Key constants (defined in `ngcloud/client.go`):
- DNS Records service ID: `111`
- Operation IDs: `create=45`, `delete=46`, `modify=90`
- Poll: up to 120 attempts × 5s, bounded by a 120s wall-clock `operationTimeout` (whichever is hit first)
- HTTP client request timeout: 120s
- Default TXT record TTL: 120s

### API Flow for DNS record operations

Both create and delete are funneled through a single `executeOperation(displayName, opType, state, pushParams)` helper:

1. Resolve the instance:
   - **create:** `getOrCreateInstance` — look up the instance by `displayName` first and reuse it; only `POST /instances` (with `serviceId` + `displayName` `"dnsrecord-<name>"`) when none exists. This avoids accumulating duplicate instances on every call. HTTP 400 "not unique" is still treated as success.
   - **delete:** look up the existing instance only (no create).
2. Retrieve instance UID (`GET /instances?serviceId=111`) — filter by `displayName`, pick the **oldest** match (`instanceConfigDtCreated` ascending), assumed to be the live record.
3. Create an operation (`POST /instanceOperations`) — extract `operationUid` from the `Location` header. On HTTP 409/400, recover and reuse the existing `operationUid` from `Location` instead of failing.
4. (create only) Fetch CFS parameters (`GET /instanceOperations/default/{svcOperationId}?fields=...`) and push them individually (`POST /instanceOperationCfsParams`). Delete needs no CFS params.
5. Run the operation (`POST /instanceOperations/{operationUid}/run`) — poll the **original** `operationUid` (the client no longer follows a child UUID from the `/run` `Location` header). HTTP 422 containing "Concurrent operations" is treated as "already running", not an error.
6. Wait via `waitForOperation` (`GET /instanceOperations/{operationUid}`) — poll until `dtFinish` is set, then succeed on `isSuccessful=true` (or a delete-confirmation `errorLog`, see quirks).

### Asynchronous execution model

`CreateTXTRecord` does not block on the full flow. It keeps an in-memory operation cache (`operations map[string]*operationState`, keyed by `displayName`, guarded by `operationsMu`):

- If an operation for the same record is already `pending`/`running`, the call **waits on that operation** instead of starting a duplicate (deduplication against cert-manager's concurrent/retried `Present()` calls).
- Otherwise it runs the real flow in a **background goroutine** and `select`s: it returns the real result if the goroutine finishes within **5 seconds**, otherwise it returns success early and lets the goroutine continue.
- Consequence: an error that occurs **after** the 5s window is logged but **not** returned to cert-manager. Cert-manager's own DNS self-check before ACME validation is the backstop. The early-return path is a deliberate workaround for cold-start workers where the deck-api stalls (see the comment in `CreateTXTRecord`).

`DeleteTXTRecord` runs synchronously (no cache/goroutine) and treats a "not found" instance as already deleted.

### Known API quirks
- `isSuccessful` is `null` (not `false`) while an operation is in progress — the poll loop waits for `dtFinish` to be set and must not exit on null.
- Delete operations always produce a platform-generated follow-up operation that finishes with `isSuccessful: false` and `errorLog` containing "Услуга удалена" ("Service deleted"). This means the record **was** successfully deleted; treat it as success. `waitForOperation` also treats `errorLog` containing "already deleted" or "not found" as success.

### Resolved: `/run` 500 "key [EXECUTABLE] doesn't exist" (historical)

> **Status (2026-06-17): RESOLVED.** The reworked `client.go` (asynchronous, idempotent
> execution — operation cache, get-or-create instance, 409/422 reuse, background goroutine) was
> tested against the live deck-api and the full create/delete flow now works end-to-end. The
> `/run` step succeeds; the `EXECUTABLE` error below is no longer observed. The detail is kept
> as historical context in case the platform-side fault recurs.

**Original status (2026-03-16):** Conformance tests failed due to a Nubes deck-api backend bug. All client-side API calls were correct (steps 1–5 all returned 2xx).

At step 6 (`POST /instanceOperations/{uid}/run`), the server fails with:
```json
{"ERROR": "key [EXECUTABLE] doesn't exist",
 "TAGCONTEXT": "/app/api/v1/resources/instance_operation_run.cfc [Line 283]"}
```

Server-side code at line 283:
```cfml
<cfset var jobData=#deserializeJson(job.filecontent)#/>
<cfset operation_url=jobData.executable.url />
```

The `job.filecontent` JSON in the platform's internal DB is missing the `executable` key. This is supposed to be populated by an internal "orchestrator" service that mediates between deck-api and Jenkins. That orchestrator has an **empty hostname** — confirmed by a secondary error seen when re-running an already-submitted operation: `"Cannot connect to the orchestrator. Host name may not be empty"` (line 341).

The svcOperation 45 itself is correctly configured — it has:
```
url: https://jenkins-master1.adl.nubes.ru/job/cloudServicesprod/job/powerdns/job/Records/job/create/view/tags/job/1.0.4/buildWithParameters
```

**What to tell Nubes support:**
- Endpoint: `POST /instanceOperations/{uid}/run`
- Error: `key [EXECUTABLE] doesn't exist` at `instance_operation_run.cfc [Line 283]`
- Root cause: `submitJob()` reads `job.filecontent` from DB; the `executable` key is absent because the orchestrator service responsible for writing it has an empty hostname (`instance_operation_run.cfc [Line 341]`)
- Service ID: 111 (DNS запись), svcOperationId: 45 (create), 46 (delete)

## Running the Conformance Test

Requires envtest binaries. Install once:
```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
~/go/bin/setup-envtest use --bin-dir /tmp/envtest-bins
```

Fill in a real API token in `testdata/ngcloud/secret.yaml`, then:
```bash
ENVTEST_DIR=/tmp/envtest-bins/k8s/1.35.0-linux-amd64
TEST_ZONE_NAME=<zone-name> \
TEST_ZONE_UID=<zone-uuid> \
TEST_ASSET_ETCD=$ENVTEST_DIR/etcd \
TEST_ASSET_KUBE_APISERVER=$ENVTEST_DIR/kube-apiserver \
TEST_ASSET_KUBECTL=$ENVTEST_DIR/kubectl \
go test ./... -v -timeout 10m
```

Notes:
- `testdata/ngcloud/secret.yaml` must **not** have a `namespace:` field (the framework injects its own test namespace).
- The zone's NS record (`ns.ngcloud.ru.`) is not publicly resolvable; the test uses `ns3.ngcloud.ru` (185.247.187.83) directly with `SetUseAuthoritative(false)`.

## Key Import Notes

- Conformance test package: `dns "github.com/cert-manager/cert-manager/test/acme"` — the
  package is located at `test/acme` (not `test/acme/dns`), but its declared package name is `dns`.

## Reference

`dns_record_create.sh` — bash reference script documenting the full deck-api interaction.
Not part of the webhook; used as the source of truth for API behaviour.
