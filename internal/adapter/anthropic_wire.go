package adapter

import "github.com/418-cloud/krayt/internal/task"

// This file is the one place in the repo allowed to encode an Anthropic-specific header name or
// endpoint (inject-claude-oauth-token-at-proxy.md, "Making the treadmill cheap"): a small
// declarative table, no logic. internal/proxy executes whatever task.InjectRule it's handed and
// knows nothing about Anthropic; internal/adapter is the only vendor-aware layer, and this is the
// only vendor-aware file in it.
//
// PROVENANCE — read before touching this table:
//
//   - ANTHROPIC_API_KEY on api.anthropic.com (strip x-api-key + authorization, set x-api-key from
//     the secret): observed live on 2026-08-14 via a real MITM run with a genuine
//     ANTHROPIC_API_KEY (run_c654e575, HUMAN_TODO.md / add-tls-mitm-credential-injection.md's
//     hardware verification) — a curl sending neither header got a 200 with a real body; the
//     mitm:false mirror (run_117d6f75) sending the same request got a 401
//     "x-api-key header is required", pinning the shape to exactly x-api-key. That run predates
//     this task but exercised precisely the request shape this table encodes, so it is reused
//     here as the P1 observation rather than re-run — see this task's HUMAN_TODO.md entry.
//
//   - CLAUDE_CODE_OAUTH_TOKEN on api.anthropic.com (strip x-api-key + authorization, set
//     authorization to "Bearer " + the secret): observed live on 2026-08-17 from TWO like-for-like
//     MITM runs of the same task through the same instrument (KRAYT_PROXY_LOG_HEADER_VALUES, §6.6)
//     — run_b408545b with a genuine subscription token, run_99bd261c with a genuine API key;
//     HUMAN_TODO.md has both recordings in full. Host and inference path were IDENTICAL in both
//     (POST /v1/messages?beta=… on api.anthropic.com), which is what chose the PRIMARY design over
//     the fallback; no refresh endpoint and no 401 appeared in either run. The auth header is the
//     difference: `authorization: Bearer <token>` where the API-key run sends `x-api-key: <key>`.
//     The token is forwarded VERBATIM — credential_len=108 on the wire matched the secrets-file
//     value's own length exactly, so the CLI does not exchange the long-lived sk-ant-oat01 token
//     for a short-lived one, which is what makes host-side translation possible at all.
//     anthropic-beta also differs — the subscription run sends oauth-2025-04-20 (on /v1/messages,
//     and alone on the /api/claude_code/* calls) where the API-key run sends context-1m-2025-08-07
//     and fallback-credit-2026-06-01; both share claude-code-20250219 and eight other flags. Under
//     SHAPE MIRRORING (below) krayt does not have to reproduce any of that: the CLI composes its own
//     list, because it knows it is on a subscription. Metering follows the credential, which settles
//     §6.14's marker: the subscription run's responses carry anthropic-ratelimit-unified-5h-*/7d-*
//     (subscription windows), the API-key run's carry anthropic-ratelimit-requests-*/tokens-*
//     (API credits).
//
// SHAPE MIRRORING — why the container gets a fake credential of the SAME KIND (owner decision,
// 2026-08-18; supersedes the "container is always API-key-shaped" rule in
// inject-claude-oauth-token-at-proxy.md, which that file records as settled — see the deviation
// note there):
//
//	The container is configured with the same env var the user supplied, carrying a placeholder
//	value. Claude Code then runs its own code path for that shape and emits every OAuth-specific
//	detail itself — oauth-2025-04-20, the beta list, the request line — and the proxy swaps
//	exactly one header value. The alternative (always configure ANTHROPIC_API_KEY and have the
//	proxy synthesize the OAuth request shape) means krayt guessing which of the API-key path's
//	beta flags an OAuth credential will accept, and re-guessing every time either path changes.
//	Mirroring makes the upstream request byte-identical to a real subscription session except the
//	token, which is the smallest possible surface for this table to be wrong about.
//
//	What mirroring gives up — "the container never learns which credential kind is in use" — was
//	never actually achieved: the subscription's own responses carry anthropic-ratelimit-unified-*
//	headers the container reads either way, and keeping the secret would mean fabricating response
//	headers. The other argument for API-key-shaping (it "removes the refresh problem") is moot for
//	env-var delivery: a CLI configured this way holds an access token and no refresh token, which
//	is exactly why both probe runs contacted no token endpoint at all.
//
// VERIFIED END TO END 2026-08-18 (run_df97fffa, with mitm:false control run_10fc027d): a live
// subscription-token run through this table's CLAUDE_CODE_OAUTH_TOKEN entry returned 200 on
// POST /v1/messages with the real 108-byte token attached host-side, while the container's own
// request carried a 28-byte placeholder and /run/secrets did not exist at all. Two things that run
// settled, both worth knowing before editing this file:
//
//	Claude Code validates credential FORMAT on neither path. The placeholder it accepted was the
//	entrypoint's own prefix-less "krayt-injected-at-host-proxy" (28 bytes) — the images predate the
//	§8.2 already-set-env-var branch, so they substituted their value for this table's. The
//	sk-ant-/sk-ant-oat01- prefixes here therefore remain insurance, not a demonstrated requirement.
//
//	Claude Code SCRUBS its credential from child-process environments (CLAUDE_CODE_CHILD_SESSION=1).
//	An agent running `env` inside the container cannot see the placeholder however the run is
//	configured, so "no credential in env" from an agent is not evidence either way — the
//	entrypoint's own startup line and the proxy's observation log are.
//
// A golden test (anthropic_wire_test.go) pins this table's exact contents, so the day a probe
// changes it, that test's diff IS the changelog of what changed.
var anthropicWireRules = map[string]anthropicWireRule{
	"ANTHROPIC_API_KEY": {
		Host:        "api.anthropic.com",
		Strip:       []string{"x-api-key", "authorization"},
		Set:         "x-api-key",
		Placeholder: AnthropicPlaceholderAPIKey,
	},
	"CLAUDE_CODE_OAUTH_TOKEN": {
		Host:        "api.anthropic.com",
		Strip:       []string{"x-api-key", "authorization"},
		Set:         "authorization",
		SetPrefix:   "Bearer ",
		Placeholder: "sk-ant-oat01-krayt-placeholder-do-not-use",
	},
}

// anthropicWireRule is one credential shape's observed wire rule: which host it talks to, which
// headers a guest-sent request must be stripped of before the real credential is attached, which
// single header the real value is set on (always the credential's OWN secrets-file key — see
// anthropicInjectRuleFor), the literal prefix that value carries (an auth scheme), and the
// non-secret stand-in the container is configured with for THIS shape. Refresh isn't here because
// neither observed shape needs one; add it the same declarative way if a future probe does.
type anthropicWireRule struct {
	Host        string
	Strip       []string
	Set         string // header name the real credential value is attached to
	SetPrefix   string // literal prefix on that header's value, e.g. "Bearer " (empty = raw)
	Placeholder string // container-facing stand-in, delivered as this shape's OWN env var
}

// AnthropicPlaceholderAPIKey is the container-facing stand-in for a real ANTHROPIC_API_KEY. Each
// shape has its own (see the table's Placeholder field) because the container mirrors the shape of
// the credential the user actually supplied — see SHAPE MIRRORING above. Exported because
// internal/cli's tests assert on the value the container is configured with.
//
// Both placeholders are deliberately self-describing, so a human who finds one in a log or
// report.md knows immediately it is not a credential (inject-claude-oauth-token-at-proxy.md §3).
// They carry the real thing's prefix (sk-ant- / sk-ant-oat01-) as cheap insurance against a
// client-side format check, NOT because one is known to exist: run_c654e575 authenticated fine with
// the entrypoint's own prefix-less "krayt-injected-at-host-proxy", which is evidence Claude Code
// does not validate the format at all. A placeholder is never added to SecretsBundle or the
// redactor set (adapter.go's Plan.Placeholders contract) — redacting it would hide the evidence
// that the run was credential-free.
const AnthropicPlaceholderAPIKey = "sk-ant-krayt-placeholder-do-not-use"

// anthropicInjectRuleFor returns the host-side injection rule for credentialKey (a secrets-file
// key name, e.g. "ANTHROPIC_API_KEY") and the placeholder env the container should get instead,
// and whether credentialKey's wire shape has actually been observed. Set always maps the rule's
// header to credentialKey ITSELF — the real value is resolved host-side from that same
// secrets-file key later (internal/orchestrator's buildEgressStdinConfig), same as any
// hand-written network.inject rule.
//
// The placeholder is keyed by credentialKey too: the container is configured with the SAME env var
// the user supplied, carrying a fake value, so the agent runs its own code path for that credential
// shape and krayt has nothing to synthesize (SHAPE MIRRORING above).
func anthropicInjectRuleFor(credentialKey string) (rule task.InjectRule, placeholders map[string]string, ok bool) {
	r, ok := anthropicWireRules[credentialKey]
	if !ok {
		return task.InjectRule{}, nil, false
	}
	out := task.InjectRule{
		Host:  r.Host,
		Strip: append([]string(nil), r.Strip...),
		Set:   map[string]string{r.Set: credentialKey},
	}
	if r.SetPrefix != "" {
		out.SetPrefix = map[string]string{r.Set: r.SetPrefix}
	}
	return out, map[string]string{credentialKey: r.Placeholder}, true
}
