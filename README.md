# Incident Enricher Pack

Reference coretexOS pack that proves the full platform loop: config overlays,
policy-gated jobs with approvals, artifacts, and a multi-step workflow.

This repo ships two things:
- A pack bundle (`pack/`) that installs into coretexOS.
- Simple workers (fetch/summarize/post/ingest) that execute the workflow.

Docs:
- `docs/overview.md` for the platform pitch and pack concepts.
- `docs/quickstart.md` for the install + demo flow.

## Build the pack bundle

```
./scripts/bundle.sh
```

## Install the pack into coretexOS

```
./scripts/install.sh
```

This registers schemas/workflows, applies pools/timeouts overlays, and installs a
safety policy fragment that requires approval for the `post` step.

## Architecture at a glance

```
Webhook / coretexctl
        |
     Gateway
        |
  Workflow Engine ----> Safety Kernel
        |                   |
     Scheduler <------------- (policy decision)
        |
      NATS (CAP bus)
        |
   Pack workers (fetch/summarize/post)
        |
     Redis (context/result pointers)
        |
    Artifacts API (audit trail)
```

## Pack contents

- `pack/pack.yaml` – pack manifest
- `pack/workflows/incident_enrich.yaml` – workflow template
- `pack/schemas/*.json` – workflow data contracts
- `pack/overlays/*.yaml` – pools/timeouts/policy fragments

## Build and run workers

Local build:

```
go build ./cmd/...
```

Run workers with Docker (coretexOS must already be running):

```
docker compose -f deploy/docker-compose.workers.yaml --env-file deploy/env.example up --build
```

`deploy/env.example` targets the Docker network (`gateway`, `nats`, `redis`). For local runs without Docker, point those to `localhost`.

## Demo

Start a run and approve the post step:

```
./scripts/demo.sh
./scripts/approve_latest_post.sh
```

If you run the ingester, set `INGESTER_URL` to post the sample webhook:

```
INGESTER_URL=http://localhost:8088 ./scripts/demo.sh
```

## Configuration summary

- `CORETEX_GATEWAY_URL`, `CORETEX_API_KEY`
- `NATS_URL`, `REDIS_ADDR` or `REDIS_URL`
- `WORKER_POOL` (per worker: `incident-enricher-fetch|summarize|post`), `WORKER_ID`, `WORKER_MAX_PARALLEL`
- `LLM_PROVIDER` (mock), `OPENAI_API_KEY`, `OPENAI_MODEL`
- `SLACK_WEBHOOK_URL`
