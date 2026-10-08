# heain-agent

The sidecar that does [heain-sdk](https://github.com/heainframework/heain-sdk)'s job for an app written in any other language (Step 5, 2026-10-08). The app talks to it over plain HTTP/JSON on loopback; the agent talks to heain-core over the App API with mTLS, exactly as a Go app built on heain-sdk would (it is heain-sdk inside).

- **Registration and the certificate:** the agent enrolls, registers, keeps the registration alive, and holds the app's key and certificate. The app never sees them.
- **Local API:** HTTP/JSON on `127.0.0.1` only, with a new token on every start, in a 0600 file.
- **Callbacks:** the agent claims each job and POSTs it to the app. It terminates the mTLS of every declared direct endpoint and forwards the request to the app.
- **The SDK's rules hold:** formal endpoints are audited automatically. A capability with `ai.used` must give a reasoning record for every job and every answer, or the job fails and the answer is refused (`reasoning_record_missing`).

It ships with a Python example (`examples/python-ref`, the standard library only) that passes the heain conformance suite through the agent. Run it however you like: a plain process, a service unit, or a container (Docker is not required).

## Author decisions (2026-10-08)

1. **Local API:** HTTP/JSON on loopback only, with a per-start token (`Authorization: Bearer`). This follows the spec's localhost exception; everything else is mTLS.
2. **Inbound work: the agent calls the app back.** A job capability maps to a path the agent POSTs claimed jobs to. A direct endpoint is served by the agent (mTLS, lanes, the user a gateway vouched for, offline rules) and forwarded to the app.
3. **AI and audit are enforced as in the SDK.** The agent signs each reasoning record and sends it to core itself.
4. **Packaging:** this open repo, with a Python example that passes C1–C14 (C12 is N/A for it).

## Running it

The manifest names the agent instead of the SDK:

```yaml
app:
  sdk: {name: heain-agent, version: ">=1.0.0"}
```

The agent reads the same environment as heain-sdk (`HEAIN_MANIFEST`, `HEAIN_INSTANCE`, `HEAIN_CORE_URL`, `HEAIN_CORE_ID`, `HEAIN_CA`, `HEAIN_STATE_DIR`, `HEAIN_ENROLL_TOKEN`, `HEAIN_LISTEN`, ...), plus `HEAIN_AGENT_CONFIG`, the agent's own configuration (default `heain-agent.json`):

```json
{
  "listen": "127.0.0.1:0",
  "app_url": "http://127.0.0.1:0",
  "command": ["python3", "app.py"],
  "ready": "/ready",
  "jobs": {"ref.upper": "/jobs/ref.upper", "ref.label": "/jobs/ref.label"}
}
```

| Field | Meaning |
|---|---|
| `listen` | The local API's address. It must be loopback (default `127.0.0.1:7600`). Port 0 picks a free port. |
| `token_file` | Where the token is written (default `<HEAIN_STATE_DIR>/agent.token`). |
| `app_url` | The app's own loopback `http` base URL, for callbacks. Port 0 with a `command`: the agent picks a free port and tells the app. |
| `jobs` | Job capability -> the path the agent POSTs claimed jobs to. |
| `endpoints` | `"METHOD /path"` from the manifest -> the app's path. Unlisted endpoints go to the same path. |
| `endpoint_listen` | Where the mTLS direct endpoints are served (default `HEAIN_LISTEN`, else `0.0.0.0:19443`). |
| `concurrency` | Jobs run at once (default 1). |
| `command` | The app to start as the agent's child. When it exits the agent stops and leaves core; when the agent stops it stops the app (SIGTERM, then SIGKILL after 10 s). |
| `ready` | A path that must answer 2xx before jobs are taken and endpoints served. |

Unknown fields are refused. A child started by `command` gets `HEAIN_AGENT_URL`, `HEAIN_AGENT_TOKEN`, `HEAIN_AGENT_TOKEN_FILE`, `HEAIN_APP_URL` and `HEAIN_APP_PORT`. Without `command`, the app reads the token from `token_file`.

## Callbacks

**A job** arrives as `POST <app_url><path>`:

```json
{"ticket_id": "...", "capability": "ref.upper", "trace_id": "...", "lane": "...",
 "lease_expires_at": "...", "payload_b64": "...", "request": "..."}
```

The app answers:

```json
{"output_b64": "...", "reasoning": [{"decision": "...", "input_b64": "...", "summary": "..."}]}
```

- `reasoning` is required for an `ai.used` capability (one record per decision; `capability` defaults to the job's).
- A non-2xx answer fails the job: it is retried, unless the status is 422 or the answer has `"permanent": true`. `error` carries the message.

**A direct endpoint** is forwarded with the method, path, query and body. The agent adds these headers:

| Header | Meaning |
|---|---|
| `X-Heain-Request` | This request's id. Send it back on every local API call made for this request. |
| `X-Heain-Trace`, `X-Heain-Lane` | The trace and lane. |
| `X-Heain-Caller` | The calling app's certificate identity. |
| `X-Heain-User`, `X-Heain-User-Roles` | The person a gateway vouched for, if any. |

The app's answer goes back to the caller as it is.

## The local API

Every call needs `Authorization: Bearer <token>`; a missing or wrong token gets 401 `unauthenticated`. A call made while handling a job or a request carries its `X-Heain-Request`, so a reasoning record, an audit event or a call to another app joins that job's or request's trace. An id that is not in progress gets 400 `unknown_request`. Errors come as `{"error": {"code": "...", "message": "..."}}`.

| Route | What it does |
|---|---|
| `GET /v1/status`, `GET /v1/mode` | The registration status and the current mode. |
| `POST /v1/jobs` | Submit a job (`capability`, `version`, `payload_b64`, `classification`, `requirements`, `idempotency_key`, ...). |
| `GET /v1/jobs/{ticket}`, `POST /v1/jobs/{ticket}/wait` | A job's state; wait for it (`?timeout_s=`, default 60). |
| `POST /v1/jobs/{ticket}/confirm-retrieval`, `.../request-erasure` | As in the SDK. |
| `GET /v1/discover?capability=&version=` | Instances of a capability. |
| `POST /v1/call` | Call another app over mTLS (`app`, `capability`, `method`, `path`, `body` or `body_b64`, `timeout_s`). |
| `POST /v1/reason` | A reasoning record (spec 04); answers `record_id`. The input is hashed in the agent and never leaves it. |
| `POST /v1/audit` | An audit event. |
| `POST /v1/policy`, `GET /v1/policy/{id}` | A P5 proposal, and its state. |
| `POST /v1/broadcast`, `GET /v1/broadcasts` | P7 knowledge broadcasts. |
| `POST /v1/journal`, `GET /v1/journal` | The offline journal. |
| `POST /v1/seal`, `POST /v1/open`, `DELETE /v1/keys/{name}` | Seal and open data with a key held in core's KMS; the key never reaches the app. |
| `GET /v1/models/{name}`, `POST /v1/models/{name}/fetch` | Approved models. |

`examples/python-ref/heain_agent.py` is a small client for these routes (the standard library only).

## Tests

- `go test ./...` covers the configuration checks, the reasoning-record mapping, the token and the request binding.
- `scripts/live_5d.sh` (run from heain-core, with `SDK_GOWORK` set) runs the Python example against a live core: admission, the token, mTLS endpoints, reasoning records, audit, relays, jobs, sealing, and both ways of stopping (19 checks).
- The conformance suite, from heain-sdk: `heain-conformance --app examples/python-ref --core <heain-core>`.

## The Python example as a container image (CineNexus Pro case study, step 2a, 2026-10-09)

Docker is one way to package an app, never a requirement. `examples/python-ref` also ships:

- `Dockerfile`: `python:3.12-slim-bookworm` with the app and heain-agent (built on the host with CGO off), the agent as the entrypoint; it starts `python3 app.py` as before.
- `conformance.docker.yaml`: the same checks as `conformance.yaml`, the app run as that image. From heain-sdk: `heain-conformance run --app examples/python-ref --conf conformance.docker.yaml`.
- **Running it by hand:** `docker run --rm --init --network host -e HEAIN_CORE_URL -e HEAIN_CORE_ID -e HEAIN_CA -e HEAIN_CHAIN -e HEAIN_MANIFEST -e HEAIN_INSTANCE -e HEAIN_STATE_DIR -e HEAIN_ENROLL_TOKEN -e HEAIN_ENDPOINT_BASE -e HEAIN_LISTEN -v <dir with those files>:<same path> heain-agent-ref:conformance` -- in the foreground (no `-d`), on the host network, so the agent reaches its node's core over loopback.
