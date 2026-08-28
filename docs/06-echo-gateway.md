# 06 — Echo Gateway

The gateway is what makes "see every integration and edit its response live" a
single feature instead of seven special cases.

## Core idea

**Every byte a workload container sends leaves through the gateway.** One choke
point, one router, one recorder, one UI.

```
                      ┌──────────── Echo Gateway ────────────┐
                      │                                       │
workload container ──►│  classify → route → record → respond  │
  AWS_ENDPOINT_URL    │      │                                │
  HTTP(S)_PROXY       │      ├──► Floci            (AWS APIs) │
                      │      ├──► sibling container (in-slice)│
                      │      ├──► mock engine   (out-of-slice)│
                      │      └──► real upstream (passthrough) │
                      └───────────────────────────────────────┘
                                      │
                                 control API :7070
                                      ▲
                                      │ SSE + REST
                                 CLI / Web UI
```

## Traffic classes

| Class | How it arrives | Default route |
| --- | --- | --- |
| AWS API calls | `AWS_ENDPOINT_URL` → gateway | forward to Floci, tap for the timeline |
| In-slice service-to-service | proxy, host matches a materialized workload | rewrite to the sibling container |
| Out-of-slice AWS-account service | proxy, host matches a *known but unmaterialized* node | mock (auto-stubbed) |
| Third-party | proxy, host matches nothing | mock (501 by default) |

The third row is the payoff of the slice model: dependencies outside the boundary
aren't missing, they're stubbed and visible. The developer sees
`ext/inventory-service — 4 calls, unmocked` and knows precisely what to define.

## Interception mechanics

### AWS SDK traffic

`AWS_ENDPOINT_URL` is honoured by AWS SDK Go v2, JS v3, boto3, Java v2, and the
AWS CLI v2. That covers the overwhelming majority of modern applications and costs
nothing.

Known gaps, documented rather than papered over:
- Old SDKs (JS v2, Java v1) need per-service endpoint overrides.
- Hardcoded `https://sqs.us-east-1.amazonaws.com/...` strings bypass it.

The v2 fallback is DNS: attach a resolver to the docker network that maps
`*.amazonaws.com` to the gateway, and terminate TLS with the generated CA. That
catches everything but adds a moving part, so it stays behind a flag until the
simple path proves insufficient.

### HTTP/HTTPS traffic

Standard forward proxy via `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`, with TLS
interception using a per-project CA generated at first `up` and stored in
`.cloud-echo/ca/`.

Making the language trust that CA is the genuinely annoying part, and cloud-echo
must handle it per runtime rather than telling users to figure it out:

| Runtime | Injection |
| --- | --- |
| Node.js | `NODE_EXTRA_CA_CERTS=/etc/cloud-echo/ca.pem` |
| Python (requests/botocore) | `REQUESTS_CA_BUNDLE`, `AWS_CA_BUNDLE` |
| Go | `SSL_CERT_FILE` |
| Java | append to a copied `cacerts`, `-Djavax.net.ssl.trustStore` |
| .NET | `SSL_CERT_FILE` (Linux) |
| curl / libcurl | `CURL_CA_BUNDLE` |

Certificate pinning defeats all of this. When the gateway sees a TLS handshake
failure it must say so explicitly — "upstream appears to pin certificates; use
`mode: passthrough` with `tls: tunnel` to bypass interception for this
integration" — rather than surfacing an opaque connection reset.

## Modes, per integration

| Mode | Behaviour | Use |
| --- | --- | --- |
| `mock` | Match rules, serve canned responses. Never leaves the machine. | Default. Offline-safe. |
| `record` | Call the real upstream, save a cassette, return the real response. | Bootstrap realistic fixtures. |
| `replay` | Serve from cassette; error on a cassette miss. | Deterministic reruns, CI. |
| `passthrough` | Proxy to the real upstream, no recording. | Rare, deliberate. |
| `fault` | Inject latency, errors, timeouts, partial responses. | Resilience testing. |

`record` and `passthrough` make real network calls with real credentials. They are
**never the default**, require explicit opt-in per integration, and are announced
loudly at startup:

```
⚠  ext/payments-api is in `record` mode — real requests WILL be sent to
   https://api.payments.example.com using your local credentials.
```

## Mock rules

```yaml
rules:
  - name: charge-succeeds
    match:
      method: POST
      path: /v1/charges
      headers: { x-tenant: acme }
      body: { jsonpath: "$.amount", gt: 0 }
    respond:
      status: 201
      body: { id: "ch_{{uuid}}", amount: "{{req.body.amount}}", status: succeeded }
      delay: 120ms

  - name: charge-declines-over-limit
    match: { method: POST, path: /v1/charges, body: { jsonpath: "$.amount", gt: 100000 } }
    respond: { status: 402, body: { error: card_declined } }
```

- First match wins; order matters and is explicit.
- Templating for echoing request data back — a mock that can't reflect the request
  is useless for anything with an ID round-trip.
- **Stateful mocks are deferred.** Sequence-dependent responses (call 1 → pending,
  call 2 → complete) are a real need but a big feature; see
  [09-open-questions.md](09-open-questions.md).

## Non-HTTP egress

"Edit the queue send" is covered because SQS/SNS are AWS API calls and therefore
already flow through the gateway. The gateway can:

- **observe** — show every `SendMessage` with its body in the timeline
- **mutate** — rewrite a message body before it reaches Floci
- **block** — drop the send and return a synthetic success or a throttling error

That last one is genuinely useful: simulating "the queue is full" or "the write
failed" without touching application code is normally very hard.

Raw TCP (a direct Postgres or Redis connection to something outside the slice)
does **not** go through the proxy. Those go to real local containers, so there is
nothing to mock. Non-AWS raw-TCP third parties are out of scope; the docs say so.

## Live editing

The control API is what makes "in real time" true:

```
GET    /api/integrations                 list, with call counts
GET    /api/integrations/:id/rules
PUT    /api/integrations/:id/rules       applied immediately, no restart
POST   /api/integrations/:id/mode
GET    /api/traffic          (SSE)       live request/response stream
POST   /api/traffic/:id/replay           re-send a captured request
POST   /api/rules/promote                write an ephemeral rule into the blueprint
```

Two layers of rules, deliberately:

- **Blueprint rules** — committed, versioned, shared with the team.
- **Session rules** — created in the UI, live in memory, gone on `down`.

`promote` moves a session rule into `cloud-echo.override.yaml`. Without this
split, either every experiment dirties git or nothing you discover is keepable.

## Performance

The gateway sits in the path of every AWS call, so it must not be the bottleneck:

- Streaming pass-through; no full-body buffering unless a rule needs the body.
- Rule matching is compiled once on load, not interpreted per request.
- The traffic timeline is a bounded ring buffer (default 1000 entries) with bodies
  truncated past 64 KB. Unbounded capture will eat all available memory on any
  chatty service.
- Target overhead: < 2 ms p99 on pass-through. If it can't hit that, make the tap
  sampled rather than making it slow.
