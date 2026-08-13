# Task: make the host egress proxy a TLS MITM that injects credentials, so secrets never enter the VM

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 egress, §6.8 secrets, §6.10 container, §6.14 agent auth,
§8.2 container contract, §10 security model) first.**

**Depends on [`move-egress-proxy-to-host.md`](./move-egress-proxy-to-host.md) (step 1). Do not start
until that has landed and its hardware re-verification is green.** This task assumes the allowlist
proxy already runs on the host in `internal/proxy`, reachable from the guest over the `EgressPort`
vsock channel.

Step 3 ([`inject-claude-oauth-token-at-proxy.md`](./inject-claude-oauth-token-at-proxy.md)) builds on
this; keep OAuth-specific logic out of here.

## Reason

Today an agent credential rides `SecretsBundle` → guest memory → container tmpfs at `/run/secrets`
(§6.8, §6.14). The agent process can read it, and so can anything that compromises the agent. §10
already lists "Auth-credential blast radius" as a residual and §6.14 recommends scoped, revocable API
keys partly because of it. A stolen credential **outlives the run**, which the ephemeral-VM model
otherwise prevents.

If the host proxy terminates TLS, it can attach the credential itself. The container then holds no
credential at all, and the ephemeral-VM blast-radius guarantee finally covers auth material too.

## Be honest about what this does and does not buy

Write these into the spec, not just the commit message. Overselling this is the main way it goes
wrong:

- **It removes credential *theft*, not credential *use*.** The proxy cannot distinguish an
  agent-initiated request from a legitimate one. A compromised agent still has unlimited
  *authenticated* access to every allowlisted host for the duration of the run. This converts
  exfiltration into a confused deputy — a real improvement, because a confused deputy dies with the
  VM and a stolen key does not, but it is not "no risk".
- **It only covers HTTP-shaped credentials.** An SSH key, a signing key, or anything a tool computes
  over cannot move to the proxy. Those still ride `SecretsBundle`.
- **It moves the adversarial parser outside the blast-radius boundary.** §6.6 already names the proxy
  "the component most directly exposed to untrusted, adversarial network input". Before step 1, a
  proxy compromise bought unrestricted egress from a VM that was about to be destroyed. After this
  task, it buys code execution in the one host process holding the user's real credentials. Go's
  memory safety helps; request smuggling and header-confusion bugs do not care. This is the price,
  and the mitigations below (§6, §7) are not optional garnish.

## Design decisions (already made — implement these, don't relitigate)

| Decision | Rationale |
|---|---|
| **Opt-in, default off** | `network.mitm: false` by default. A trust-model change must not arrive by upgrade. |
| **`mitm` is allowed in every mode, `full` included** | In `full` + `mitm`, every TLS connection not in `passthrough` is intercepted, including hosts that appear in no allowlist. That is the point of `full`, but it means leaf-cert generation is unbounded — so the SNI cache **must** be capped (see §3) rather than growing for the run's lifetime. Say plainly in the docs that `full` + `mitm` intercepts everything. |
| **Ephemeral per-run CA, in memory, never written to host disk** | A persistent krayt CA on the user's disk that VMs trust is a worse artifact than the one we removed. Generate at run start, discard at teardown. |
| **ECDSA P-256** for CA and leaves | Per-connection RSA keygen is visibly slow. Cache leaves by SNI, **bounded** (see above). |
| **Secret material reaches the proxy child on stdin, never argv or env** | Step 1 runs the proxy as a separate process (`krayt __egress-proxy`). Flags land in the process table and env is readable from `/proc/<pid>/environ`; a JSON blob written to the child's stdin at startup, then closed, is neither. |
| **`http/1.1` only in ALPN** | A hijacked `CONNECT` does **not** get `net/http`'s automatic h2 upgrade; advertising `h2` and then serving 1.1 breaks clients. Upstream keeps h2 via `Transport.ForceAttemptHTTP2`. |
| **`FlushInterval: -1`** | `ReverseProxy` only auto-flushes `text/event-stream`. Streaming NDJSON and long-poll would otherwise buffer and stutter the agent's token stream. |
| **Per-host passthrough (tunnel, no MITM) list** | Pinned clients and non-HTTP-over-TLS (git+ssh on 443) must survive. Those hosts get no injection, by definition. |
| **Never log request or response bodies** | Every byte is now cleartext in a process that writes logs. Headers may be logged **name-only**. |
| **`net/http/httputil.ReverseProxy`, stdlib only** | No new dependency. Note for the record: §6.6's `elazarl/goproxy` option was never adopted — the current proxy is already hand-rolled, so this removes no third-party framework. |

## Implement

### 1. Task config and plumbing

`internal/task/spec.go` — extend `NetworkPolicy`:

```yaml
network:
  mode: allowlist
  allow: [api.anthropic.com]
  mitm: true                      # default false
  passthrough: [github.com]       # tunnel these, never MITM (subset of allow)
  inject:
    - host: api.anthropic.com     # exact host match, same matcher as `allow`
      strip: [x-api-key, authorization]   # remove these from the guest's request first
      set:                                # then set these
        x-api-key: ANTHROPIC_API_KEY      # header name -> secrets-file key
```

**`strip` and `set` are separate lists on purpose.** The header the container sends is not necessarily
the header that goes upstream — step 3 relies on removing one auth header and setting a *different*
one, so a single `header:` field would not be enough. `set` is a map so one rule can attach several
headers (an auth header plus a static opt-in header, say). A bare `set` with no `strip` is a valid
rule; `strip` defaults to the key set of `set`.

Values in `set` are **secrets-file key names**, resolved host-side. To attach a fixed non-secret value,
use a `set_literal` map instead — keep the two syntactically distinct so a literal can never be
mistaken for a resolved secret or vice versa.

Validation, all fail-fast at `krayt run` pre-flight (before any VM or image work):

- `inject` requires `mitm: true`.
- Every `inject[].host` must **not** be in `passthrough`. In `mode: allowlist` it must also be in
  `allow`; in `mode: full` there is no list to check against, so accept any host.
- Every secrets-file key named in `inject[].set` must exist — a typo must not silently produce an
  unauthenticated run that fails opaquely 30s into the agent.
- `passthrough ⊆ allow` in `mode: allowlist`; in `mode: full`, `passthrough` is free-form.
- Header names: reject anything that is not a valid token, and reject hop-by-hop headers.
- Injection targets **HTTPS only**. Refuse a rule whose host is reached over plain HTTP — attaching a
  credential to a cleartext request is a footgun, and the MITM path does not cover it anyway.

### 2. Secrets partitioning — the load-bearing change

An injected secret must be **withheld from `SecretsBundle`**. If it still ships to the guest, this
whole task is theater.

- `internal/orchestrator/orchestrator.go` (`pushSecrets`, around `:370`): filter out every
  secrets-file key named in any `network.inject[].set` before building the `SecretsBundle`.
- Add a test that asserts, for a spec with injection configured, the `SecretsBundle` sent over the
  wire contains **no** injected key — assert on the captured proto message, not on downstream effects.
- Keep the injected values in the host `Redactor` set used for run logs, `report.md`, and the
  `proxy.log` artifact step 1 added (§6.8).
- Record in `meta.json`/`report.md` **which keys were injected host-side** (names only), so the human
  reviewing the run knows the container ran credential-free. This is the user-visible payoff; surface
  it.

### 2b. Getting secrets into the proxy child

Step 1 runs the proxy as a separate process spawned by the run supervisor (`krayt __egress-proxy`,
listener on fd 3, `--mode`/`--allow`/`--dns` flags). Secret values must **not** join those flags.

- The supervisor writes a single JSON document to the child's **stdin** at startup and then closes it:
  the injection rules with their resolved values, and the passthrough list. The child reads to EOF
  before serving.
- Non-secret policy stays on flags; do not migrate it. Extend the documented child contract in §6.6
  (the interface a `KRAYT_EGRESS_PROXY_BIN` replacement must honor) with the stdin schema.
- The child must never write the config back out — not to `proxy.log`, not to an error message. Assert
  this with a test that plants a value and greps the child's full output.

### 3. `internal/proxy/mitm` — the CA and leaf certs

New file(s) in `internal/proxy` (keep the package OS-agnostic and unit-testable):

- `newCA()` → in-memory ECDSA P-256 self-signed CA, `IsCA: true`, `KeyUsageCertSign`, short validity
  (run duration + slack), CN naming it clearly as krayt + the run ID.
- `leafFor(sni string)` → cached, ECDSA P-256, `DNSNames: [sni]` (plus IP SANs when the CONNECT
  authority is an IP literal), signed by the run CA. Cache in a mutex-guarded map keyed by SNI;
  generate on miss. **Bound the cache** (a simple cap with LRU or random eviction, e.g. 1024 entries):
  under `mode: full` the set of SNIs is attacker-chosen and unbounded, so an uncapped map is a
  guest-triggerable host memory-growth path. Unit-test the eviction.
- `tls.Config{ GetCertificate: …, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12 }`.
- Expose `CACertPEM() []byte` — **public cert only**. There must be no code path that can serialize
  the CA private key.

### 4. The MITM path in the handler

In `internal/proxy`, extend `handler.connect` (currently a straight tunnel at `proxy.go:211`):

1. Allowlist check runs first, exactly as today. Unchanged.
2. If `!mitm` or the host is in `passthrough` → existing tunnel behavior, unchanged. **Preserve this
   path verbatim**; it is the fallback when anything about MITM misbehaves.
3. Otherwise: hijack, write `200 Connection established`, wrap the client conn in `tls.Server` with
   the leaf for the CONNECT authority, then serve HTTP/1.1 on it (`http.Serve` over a one-shot
   listener wrapping the single `tls.Conn`, or an equivalent single-conn server).
4. The handler for that inner server is an `httputil.ReverseProxy`:
   - `Rewrite` sets the outbound URL from the CONNECT authority + the inner request's path. Do **not**
     take the target host from a guest-supplied `Host` header — take it from the CONNECT authority the
     allowlist already approved.
   - `Transport` is the **existing** transport, so `checkDialAddr` still runs via the dialer `Control`
     hook on every upstream dial. The SSRF guard must not be bypassed by the MITM path — add a test
     that proves it.
   - `FlushInterval: -1`.
   - `ErrorHandler` returns a clear 502 and logs the reason server-side (mirroring the existing
     `log.Printf` at `proxy.go:223`), never the body.
5. Apply injection after the rewrite, in this order: **delete every header named in `strip`**, then
   set every header in `set`/`set_literal`. Stripping before setting is what makes the guest unable
   to influence or smuggle a second credential. Step 3 depends on `strip` and `set` naming *different*
   headers, so do not collapse them into a single delete-then-set-same-name operation.

6. **Optional per-rule `refresh` block, executed host-side.** A rule may carry a declarative
   description of an upstream credential-refresh endpoint:
   ```
   refresh: { host, path_prefix, response_token_fields: [...] }
   ```
   When present, the proxy (a) recognizes a request to that host+path as a refresh exchange, and
   (b) on an upstream `401` for the rule's host, performs at most **one** host-side refresh and
   retries the original request **once**. Never loop: a second `401` is surfaced as-is.
   The proxy stays **generic** — it executes the block, it does not know what Anthropic is. All
   vendor-specific values come from the adapter (§6.14 puts agent-specific knowledge in the adapter,
   not the core). Step 3 is the first consumer; ship the plumbing here only if it costs little,
   otherwise leave a clearly-named seam and let step 3 fill it in.

### 5. Delivering the CA to the container

The container must trust the run CA. The credential never enters the VM; the **public** CA cert must.

- Add a `ca_cert` (bytes) field to the `NetworkPolicy` proto (`internal/protocol/krayt.proto`) — it is
  per-task network config and rides the path that already exists. Regenerate with `make proto` and
  **commit** the generated files (`CLAUDE.md`).
- Guest side (`internal/guest/proxy/controller_linux.go`): write it to a tmpfs path
  (`/run/krayt/ca.crt`, `0644` — it is public), and add to the returned env map:
  - `KRAYT_CA_CERT=/run/krayt/ca.crt` — the contract.
  - Best-effort `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS` pointing at it.
- **The distro-specific part belongs in the container entrypoint** (§8.2), not the guest. `SSL_CERT_FILE`
  *replaces* the system bundle for Go/OpenSSL rather than appending, which breaks verification for
  anything on the `passthrough` list. So each `images/agents/*/entrypoint.sh` must, when
  `KRAYT_CA_CERT` is set, concatenate its own distro bundle (`/etc/ssl/certs/ca-certificates.crt` on
  the Debian-based images) with the krayt CA into one file and point the vars at **that**.
  `NODE_EXTRA_CA_CERTS` is genuinely additive and can point at the krayt CA directly.
- **Node does not read the system trust store.** All three current agent images
  (`images/agents/{claude-code,gemini-cli,opencode}`) are node-based, so `NODE_EXTRA_CA_CERTS` is not
  optional for any of them. Update all three entrypoints.
- Document the `KRAYT_CA_CERT` contract in §8.2 so third-party images can comply, and make it a no-op
  when unset (i.e. `mitm: false` runs are byte-identical to step 1).

### 6. Treat guest input as hostile

The proxy now parses attacker-controlled HTTP inside attacker-controlled TLS, on the host, holding
real credentials. Non-negotiable:

- **Strip before injecting.** Delete any inbound instance of an injected header name (§4.5). A guest
  must not be able to influence the injected value, or to smuggle a second one.
- **Bound the request.** `ReadHeaderTimeout` (already set at `proxy.go:52`), plus `MaxHeaderBytes` and
  an overall request timeout on the inner server.
- **Never reflect guest headers into the injected set**, and never let a guest header choose the
  upstream host, scheme, or port.
- **Never log bodies.** Header logging is name-only. Add a lint-visible comment at the log sites.
- Reject `CONNECT` authorities that are not a valid host[:port]; reject requests whose inner `Host`
  disagrees with the CONNECT authority (a smuggling signal) with a 400.
- Keep the run CA private key in memory only, in one place, behind an accessor that returns only PEM
  of the certificate.

### 7. Failure behavior

- Any MITM setup failure for a host (leaf generation, handshake) → **fail the connection**, do not
  silently fall back to a plain tunnel. A silent fallback would drop injection and send the agent out
  unauthenticated, producing a confusing failure far from the cause.
- An injected secret that is missing at request time is a programming error (pre-flight validated in
  §1) — 500 and log it, do not send an unauthenticated request upstream.

## Tests (offline, `httptest`-based, no VM)

- **CA/leaf**: leaf chains to the CA, SNI matches, cache returns the same leaf for repeat SNI,
  distinct leaves for distinct SNI, the cache evicts at its cap under a flood of distinct SNIs, and
  the CA private key is not reachable through any exported API.
- **Child config**: a secret value delivered on stdin never appears in the child's argv, environ, or
  full stdout/stderr output.
- **`mode: full` + `mitm`**: an unlisted host is intercepted and reaches upstream; a `passthrough`
  host in `full` is still tunneled un-MITM'd; injection still fires only for named hosts.
- **Injection**: request through the MITM path arrives upstream with the injected header; a
  guest-supplied header of the same name is **replaced, not appended**; a non-matching host gets no
  injection.
- **Passthrough**: a `passthrough` host is tunneled, upstream sees the client's original TLS (assert
  via a TLS-terminating `httptest.NewTLSServer` and cert introspection), and no header is injected.
- **SSRF guard still applies on the MITM path**: a MITM'd host resolving to a blocked IP is refused
  403 and never dialed.
- **Streaming**: an SSE and a chunked-NDJSON upstream stream through with per-chunk flushing (assert
  timing/incremental delivery, not just the final body).
- **Secrets partitioning**: `SecretsBundle` for an injection-configured spec omits the injected keys.
- **`mitm: false` is byte-identical to step 1**: same tunnel path, no CA in the env map.
- **Hostile input**: oversized headers rejected; inner `Host` ≠ CONNECT authority rejected; smuggled
  duplicate injected header stripped.
- Pre-flight validation: every rule in §1 has a failing-config test.

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

## On hardware (`[HUMAN]` — real Mac + Linux/KVM, and a live credential)

Write these as real tests, then hand off via `HUMAN_TODO.md` per §14. **Do not fabricate results.**

- A real `claude-code` image run with `mitm: true` and `inject:` for `ANTHROPIC_API_KEY`: the agent
  completes a task, and `env` + `/run/secrets` inside the container contain **no** credential. Assert
  the absence, not just the success.
- The same run with `mitm: false` still works unchanged (regression).
- `npm install` (or an equivalent TLS-heavy fetch) through the MITM path succeeds in each of the three
  agent images — this is the `NODE_EXTRA_CA_CERTS` check and it is the most likely thing to break.
- Re-run the full Phase 3 security suite on both backends; `TestEgressEnforcement` and
  `TestSecretConfinementInArtifacts` in particular.

## Docs (required)

- **§6.6** — the MITM mode, the ephemeral CA, ALPN/h2 decision, passthrough, and the "hostile guest
  input" rules from §6.
- **§6.8** — secrets partitioning: an injected secret is host-only and never enters `SecretsBundle`;
  it stays in the redactor set.
- **§6.14** — injection as the preferred delivery for HTTP-shaped agent credentials; keep the existing
  guidance for everything else.
- **§8.1** — the new `network.mitm` / `passthrough` / `inject` config keys, and that `mitm` combines
  with every mode — spelling out that `full` + `mitm` intercepts **every** TLS connection the agent
  makes except those listed in `passthrough`.
- **§8.2** — the `KRAYT_CA_CERT` container contract and what a compliant entrypoint must do.
- **§10** — new trust-boundary row and an honest residual covering all three bullets from "Be honest
  about what this does and does not buy" above.
- **`README.md`** — a short "credential injection" section with the worked `krayt.yaml`.
- **`docs/ai-tasks/README.md`** — status.

## Done when

- With `mitm: true` + `inject:`, a real agent run authenticates successfully while the container's
  environment and `/run/secrets` hold no credential — asserted, on hardware.
- With `mitm: false` (the default) behavior is identical to step 1.
- The `SecretsBundle` omits injected keys, guarded by a test on the captured proto message.
- Passthrough hosts are tunneled un-MITM'd; the SSRF guard applies on both paths.
- The CA private key never leaves memory and is never serializable through an exported API.
- Spec §6.6/§6.8/§6.14/§8.1/§8.2/§10 updated, including the honest limitations.

## Constraints

- Stdlib only (`crypto/tls`, `crypto/x509`, `crypto/ecdsa`, `net/http/httputil`). No new dependency.
- `internal/proxy` stays OS-agnostic, build-tag-free, and unit-testable with no VM and no network.
- Keep the existing `Factory` / `newHandler` seams; MITM is a mode of the existing handler, not a fork
  of it.
- The plain-tunnel path must remain intact and reachable — it is the fallback and the passthrough
  implementation.
- Default off. A user who does not opt in must observe zero behavior change.
