# Repo: `incident-enricher-pack`

## Goals

* Prove pack install/uninstall works end-to-end.
* Prove scheduler routing overlays work (pools).
* Prove safety policy fragments load from config service and can **force approvals**.
* Prove artifacts API works (store evidence + final summary).
* Provide a clean upgrade path to “SRE Investigator”.

Packs are explicitly meant to extend the platform **without core code changes** and with **no arbitrary code execution during install**.

---

# 1) Top-level structure

You want a clean separation between:

* **Pack bundle** content (what `coretexctl pack install` consumes)
* **Worker source + deploy** content (how you run workers)

Recommended repo layout:

```
incident-enricher-pack/
  README.md
  Makefile
  LICENSE

  pack/                          # <-- bundle root (tar this)
    pack.yaml
    workflows/
      incident_enrich.yaml
    schemas/
      IncidentInput.json
      EvidenceBundle.json
      Summary.json
      PostResult.json
    overlays/
      pools.patch.yaml
      timeouts.patch.yaml
      policy.fragment.yaml

  cmd/                           # Go binaries
    fetcher/
      main.go
    summarizer/
      main.go
    poster/
      main.go
    ingester/                    # OPTIONAL but recommended
      main.go

  internal/                      # shared libraries
    gatewayclient/
      client.go
    artifacts/
      artifacts.go
    incidents/
      mock.go
      pagerduty.go               # optional
    llm/
      openai.go                  # optional
      mock.go
    slack/
      webhook.go                 # optional
    policyconstraints/
      parse.go

  deploy/
    docker-compose.workers.yaml
    env.example

  scripts/
    bundle.sh
    install.sh
    demo.sh
    approve_latest_post.sh

  testdata/
    sample_incident.json
    sample_webhook.json
```

**Key rule:** your release artifact for installation is only `pack/` bundled as `.tgz`. The rest is dev/runtime convenience.

---

# 2) Pack identity + namespacing

Pack ID: `incident-enricher`

Naming rules you already documented:

* Topics: `job.<pack_id>.*`
* Workflow IDs: `<pack_id>.<name>`
* Schema IDs: `<pack_id>/<name>`
* Pool names: must start with `<pack_id>`

Also: Safety kernel denies anything that isn’t a `job.` topic, so don’t fight it.

---

# 3) Topics + capabilities (the contract)

Use exactly 3 job topics:

| Step          | Topic                             | Capability           | riskTags                               | requires                                   |
| ------------- | --------------------------------- | -------------------- | -------------------------------------- | ------------------------------------------ |
| Fetch context | `job.incident-enricher.fetch`     | `incident.fetch`     | `["network"]`                          | `["network:egress"]`                       |
| Summarize     | `job.incident-enricher.summarize` | `incident.summarize` | `["network"]` *(or none if local LLM)* | `["llm"]`                                  |
| Post          | `job.incident-enricher.post`      | `incident.post`      | `["network","write"]`                  | `["slack:webhook"]` *(or “artifact-only”)* |

These meta fields are actually used by policy evaluation (capability / risk tags / requires / pack id / idempotency key).

---

# 4) `pack/pack.yaml` (concrete spec)

Use your v0 schema, but keep it tight and deterministic.

```yaml
apiVersion: coretexos.dev/v1alpha1
kind: Pack

metadata:
  id: incident-enricher
  version: 0.1.0
  title: Incident Enricher
  description: Fetch context, summarize, require approval before posting.

compatibility:
  protocolVersion: 1
  minCoreVersion: 0.6.0

topics:
  - name: job.incident-enricher.fetch
    capability: incident.fetch
    riskTags: ["network"]
    requires: ["network:egress"]

  - name: job.incident-enricher.summarize
    capability: incident.summarize
    riskTags: ["network"]
    requires: ["llm"]

  - name: job.incident-enricher.post
    capability: incident.post
    riskTags: ["network", "write"]
    requires: ["slack:webhook"]

resources:
  schemas:
    - id: incident-enricher/IncidentInput
      path: schemas/IncidentInput.json
    - id: incident-enricher/EvidenceBundle
      path: schemas/EvidenceBundle.json
    - id: incident-enricher/Summary
      path: schemas/Summary.json
    - id: incident-enricher/PostResult
      path: schemas/PostResult.json

  workflows:
    - id: incident-enricher.enrich
      path: workflows/incident_enrich.yaml

overlays:
  config:
    - name: pools
      scope: system
      key: pools
      strategy: json_merge_patch
      path: overlays/pools.patch.yaml

    - name: timeouts
      scope: system
      key: timeouts
      strategy: json_merge_patch
      path: overlays/timeouts.patch.yaml

  policy:
    - name: safety
      strategy: bundle_fragment
      path: overlays/policy.fragment.yaml

tests:
  policySimulations:
    - name: allow_fetch
      request:
        tenantId: default
        topic: job.incident-enricher.fetch
        capability: incident.fetch
        riskTags: ["network"]
      expectDecision: ALLOW

    - name: require_approval_post
      request:
        tenantId: default
        topic: job.incident-enricher.post
        capability: incident.post
        riskTags: ["network","write"]
      expectDecision: REQUIRE_APPROVAL
```

---

# 5) Workflow definition

You can structure the workflow in whatever your workflow DSL expects, but the **semantic contract** should be:

**Input**: `IncidentInput`
**Step 1** `fetch` → outputs `EvidenceBundle` (with artifact pointers)
**Step 2** `summarize` → outputs `Summary` (and optionally summary artifact ptr)
**Step 3** `post` → outputs `PostResult`
…and **Step 3 must get blocked by safety approval**.

Important: the platform already supports job-level “requires human approval”:

* Safety kernel returns `DecisionType_REQUIRE_HUMAN` on `require_approval`.
* Scheduler converts “approval required” into `SafetyRequireApproval` and sets the job state to `JobStateApproval`.

So you don’t need a workflow “approval step” to prove approvals. You just need policy to require approval for the **post** job.

Also: step metadata can carry `pack_id`, `capability`, `risk_tags`, `requires`, etc., and maps into dispatch metadata.

### Minimal conceptual workflow (pseudo-YAML)

```yaml
id: incident-enricher.enrich
name: Incident Enricher
version: "0.1.0"
input_schema: incident-enricher/IncidentInput

steps:
  fetch:
    type: worker
    topic: job.incident-enricher.fetch
    meta:
      pack_id: incident-enricher
      capability: incident.fetch
      risk_tags: ["network"]
      requires: ["network:egress"]
    output_schema: incident-enricher/EvidenceBundle

  summarize:
    type: worker
    topic: job.incident-enricher.summarize
    after: [fetch]
    meta:
      pack_id: incident-enricher
      capability: incident.summarize
      risk_tags: ["network"]
      requires: ["llm"]
    input:
      evidence: ${steps.fetch.output}
    output_schema: incident-enricher/Summary

  post:
    type: worker
    topic: job.incident-enricher.post
    after: [summarize]
    meta:
      pack_id: incident-enricher
      capability: incident.post
      risk_tags: ["network","write"]
      requires: ["slack:webhook"]
    input:
      incident: ${input}
      evidence: ${steps.fetch.output}
      summary: ${steps.summarize.output}
    output_schema: incident-enricher/PostResult
```

---

# 6) Schemas (full contract)

Keep these strict-ish. It prevents “demo success, production pain”.

## `pack/schemas/IncidentInput.json`

Key idea: support both webhook-style and manual-trigger style.

Minimal fields:

* `incident_id` (string, required)
* `title` (string)
* `severity` (string enum)
* `source` (object) e.g. `{system:"mock"|"pagerduty", url:"..." }`
* `raw` (object) (the webhook payload)
* `destination` (object) e.g. `{mode:"artifact"|"slack", slack_webhook_url:"..."}`

## `pack/schemas/EvidenceBundle.json`

* `incident_id`
* `evidence` array:

  * `{kind, title, artifact_ptr, content_type, bytes}`
* `normalized_context` object (optional)
* `collected_at`

## `pack/schemas/Summary.json`

* `incident_id`
* `summary_md` (string)
* `highlights` (array)
* `action_items` (array)
* `confidence` (0..1 optional)
* `model` (string optional)
* `artifact_ptr` (optional)

## `pack/schemas/PostResult.json`

* `incident_id`
* `mode` (slack|artifact)
* `slack` object (optional): `{ok, channel, ts, permalink}`
* `artifact_ptr` (optional)
* `posted_at`

---

# 7) Config overlays

Your pack doc already defines overlays and limits supported keys to `pools` and `timeouts` with json merge patch semantics, and `null` deletes keys (for uninstall).

## `pack/overlays/pools.patch.yaml`

Goal:

* Route each topic to its own dedicated pool so the scheduler targets the right worker.

Example:

```yaml
pools:
  incident-enricher-fetch:
    requires: ["network:egress"]
  incident-enricher-summarize:
    requires: ["llm"]
  incident-enricher-post:
    requires: ["slack:webhook"]

topics:
  job.incident-enricher.fetch: incident-enricher-fetch
  job.incident-enricher.summarize: incident-enricher-summarize
  job.incident-enricher.post: incident-enricher-post
```

Workers must heartbeat into their respective pools to receive jobs.

## `pack/overlays/timeouts.patch.yaml`

Keep first version conservative:

```yaml
topics:
  job.incident-enricher.fetch:
    timeout_seconds: 30
    max_retries: 2
  job.incident-enricher.summarize:
    timeout_seconds: 90
    max_retries: 1
  job.incident-enricher.post:
    timeout_seconds: 20
    max_retries: 1
```

---

# 8) Safety policy fragment (the main “wow”)

The safety kernel supports merging policy fragments from config service; the loader:

* pulls a doc from config service (scope/id/key are configurable by env vars),
* expects `doc.Data[configKey]` to be a map (bundle name → fragment),
* extracts fragment content as a string (it supports raw string or map with `"content"` / `"policy"` / `"data"` keys),
* and merges fragments by **appending rules** and combining tenant allow/deny lists.

It defaults to:

* `SAFETY_POLICY_CONFIG_SCOPE=system`
* `SAFETY_POLICY_CONFIG_ID=policy`
* `SAFETY_POLICY_CONFIG_KEY=bundles`
* reload interval default `30s`.

So your fragment should:

* allow fetch + summarize
* require approval for post

### `pack/overlays/policy.fragment.yaml` (recommended minimal)

**Important note:** I’m using a generic “rules” shape consistent with `Decision=allow/deny/require_approval/...` behavior in the kernel. If your policy YAML has slightly different field names, adjust, but keep semantics.

```yaml
version: 1
default_tenant: default

tenants:
  default:
    allow_topics:
      - "job.incident-enricher.*"

rules:
  - id: incident-enricher-allow-fetch
    match:
      topic: "job.incident-enricher.fetch"
    decision: allow

  - id: incident-enricher-allow-summarize
    match:
      topic: "job.incident-enricher.summarize"
    decision: allow

  - id: incident-enricher-require-approval-post
    match:
      topic: "job.incident-enricher.post"
    decision: require_approval
    reason: "Posting externally requires explicit approval"
```

### Why this proves the platform

Once installed, when the workflow hits `job.incident-enricher.post`, the scheduler will put that job into `JobStateApproval` and stop, because safety says approval is required.

Then you approve via Gateway approvals endpoints:

* list approvals: `GET /api/v1/approvals`
* approve: `POST /api/v1/approvals/{job_id}/approve`
* reject: `POST /api/v1/approvals/{job_id}/reject`.

---

# 9) Workers (binaries + behavior)

## Shared runtime requirements

All workers need:

* NATS (for CAP bus)
* Redis (for pointers/state)
* Gateway URL + API key (for artifacts, starting runs, etc.)

Gateway uses API key auth with `X-API-Key` header or `api_key` query parameter.

### Worker should consume policy constraints (forward-thinking)

Scheduler injects policy constraints into job env:

* `CORETEX_POLICY_CONSTRAINTS` (protojson-encoded constraints)
* `CORETEX_REDACTION_LEVEL` if set
* plus budget env like `CORETEX_MAX_RETRIES`, etc..

This is *perfect* for your first pack to demonstrate “governance is enforceable”:

* Summarizer respects `CORETEX_REDACTION_LEVEL` (redact secrets or truncate).
* Poster enforces network allowlist or refuses to post if constraints disallow it.

## `cmd/fetcher`

Input: `IncidentInput`
Output: `EvidenceBundle`

Behavior:

* If `source.system == "mock"`: treat `raw` as the incident, normalize it.
* If `"pagerduty"` (optional): GET incident details, notes, responders.
* Store:

  * raw payload as artifact
  * normalized context as artifact (optional)
* Return `EvidenceBundle` referencing artifacts.

Artifacts API:

* `POST /api/v1/artifacts` accepts `content` or `content_base64`, `content_type`, and `retention` (short/audit/standard) and returns `artifact_ptr`.

## `cmd/summarizer`

Input: `EvidenceBundle`
Output: `Summary`

Behavior:

* load evidence artifacts (optional)
* produce summary markdown + bullets
* store summary as artifact
* attach model metadata
* if LLM disabled, use `mock` summarizer (static deterministic summary).

## `cmd/poster`

Input: `IncidentInput` + `Summary` (+ optional `EvidenceBundle`)
Output: `PostResult`

Behavior:

* If `destination.mode == "artifact"`: write a “post payload” artifact and exit.
* If `destination.mode == "slack"`:

  * enforce approval exists (implicitly handled by scheduler/policy gate)
  * post to Slack webhook
  * store Slack response + permalink (if you can compute it) or at least `ok/ts`
  * store posted message as artifact (audit trail)

Idempotency:

* For Slack posting, use `incident_id` as the idempotency key (store a Redis key like `incident-enricher:posted:<incident_id>`). Don’t double-post if job retries.

## `cmd/ingester` (optional but recommended)

This solves the “we don’t have webhook ingress” problem cleanly.

* Expose `POST /webhook/mock` and `POST /webhook/pagerduty`
* Transform incoming payload → `IncidentInput`
* Start a workflow run via Gateway:

Gateway start run endpoint:

* `POST /api/v1/workflows/{workflow_id}/runs`
* supports idempotency via `Idempotency-Key` header.

---

# 10) Deploy spec (compose)

## `deploy/env.example`

```
CORETEX_GATEWAY_URL=http://gateway:8080
CORETEX_API_KEY=devkey

NATS_URL=nats://nats:4222
REDIS_ADDR=redis:6379

WORKER_POOL=incident-enricher-fetch  # overridden per-service in compose

# summarizer
LLM_PROVIDER=mock
OPENAI_API_KEY=
OPENAI_MODEL=gpt-4o-mini

# poster
SLACK_WEBHOOK_URL=
```

## `deploy/docker-compose.workers.yaml`

Runs only the pack services; you run coretex separately (or extend this file with core services if you want a single compose).

Services:

* fetcher
* summarizer
* poster
* ingester (optional)

---

# 11) Scripts (DX that matters)

## `scripts/bundle.sh`

* creates `dist/incident-enricher-0.1.0.tgz` from `pack/`
* prints sha256

## `scripts/install.sh`

* `coretexctl pack install ./dist/incident-enricher-0.1.0.tgz --upgrade`

## `scripts/demo.sh`

* posts a sample webhook to ingester OR calls gateway start run directly
* polls workflow run status
* when it hits approval, calls `scripts/approve_latest_post.sh`

## `scripts/approve_latest_post.sh`

* `curl GET /api/v1/approvals`
* find the newest approval where `topic == job.incident-enricher.post`
* `curl POST /api/v1/approvals/{job_id}/approve`

Approvals endpoints are explicitly present.

---

# 12) CI / release spec (minimum viable)

## GitHub Actions

* `go test ./...`
* `go vet ./...`
* build docker images (fetcher/summarizer/poster/ingester)
* `scripts/bundle.sh` (upload `.tgz` as artifact)
* optionally publish container images + pack tarball as a GitHub Release

## Pack verification gate

* run `coretexctl pack verify incident-enricher` after install in an integration environment
* at least run the `policySimulations` in pack.yaml (they’re cheap and validate policy merge + safety kernel load).

---

# 13) One blunt recommendation (prevents pain)

**Do not version policy fragment keys in the config bundle** unless your upgrader actively deletes old keys.

Because policy fragments are merged by appending rules/tenants, leaving old versions behind can lead to duplicate/conflicting rules and “why is it still requiring approval?” bugs.

So: store policy fragment under a stable name like:

* `incident-enricher/safety`

…and keep the pack version in the pack registry record, not in the policy bundle key.

---

If you want, I can also give you **exact file contents** for the 4 JSON schemas + a working mock worker skeleton (same module, 4 `cmd/` binaries) that calls `/api/v1/artifacts` and `/api/v1/workflows/{id}/runs` using `X-API-Key`.
