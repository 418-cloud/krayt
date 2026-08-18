package proxy

// The opt-in request-observation log (§6.6): with Policy.LogRequests on, the proxy records one
// line per request it handles — request line, host, and header NAMES only, never a header value
// and never a body, the same rule §6.6.1 sets for every other log in this package.
//
// Why this is a mode and not always-on: proxy.log's standing job is to preserve a DENIAL's real
// reason (net/http's CONNECT-proxy client discards the response body on a non-2xx CONNECT, so the
// proxy's own log is the only witness — §6.6), which means a fully SUCCESSFUL run correctly
// produces an empty proxy.log. That is right for operators and useless as an instrument: an empty
// file cannot answer "which host, path, and auth header did this agent actually use", which is
// exactly what a wire-format probe (docs/ai-tasks/inject-claude-oauth-token-at-proxy.md, P1–P4)
// must observe before an injection rule may be written for a credential shape. This mode turns the
// proxy into that instrument for one run, without making every ordinary run persist the hosts and
// paths an agent visited.
//
// Policy.LogHeaderValues goes one step further for named headers only — see observer.headerValues,
// which is where the "never a value" rule is deliberately, narrowly relaxed and where the guard
// that keeps a credential out of the log lives.
//
// Same lint-visible rule as connect_mitm.go: nothing here logs a body, and nothing logs a
// credential. Guest-controlled text (path, query names, the CONNECT authority, an opted-in header
// value) is %q-quoted, because these lines land in a file a human reads and a hostile guest must
// not be able to forge a log line with a bare newline. Header names need no quoting — http.Server
// has already rejected anything that is not an RFC 7230 token — but they are lowercased for
// stable, greppable output.

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// credentialHeaders are header names whose values are NEVER logged in full, however explicitly
// they are opted in via Policy.LogHeaderValues — they are reduced to credentialShape instead. This
// is generic HTTP credential hygiene, not vendor knowledge (this package stays free of that, §6.6):
// every name here is a standard or near-universal place a bearer token, key, or session lands.
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
}

// observer is one proxy process's request-observation configuration. A nil *observer is the
// default — observation off, so the only lines in proxy.log are failures and denials.
type observer struct {
	// headerValues names the headers whose VALUES may appear in the log, lowercased. Populated
	// from Policy.LogHeaderValues, i.e. only ever by an operator naming them explicitly for one
	// probe run. This exists because a header NAME is not always enough: an API's required opt-in
	// flags (a beta/version header) are non-secret facts a probe must record exactly, and guessing
	// them is what inject-claude-oauth-token-at-proxy.md forbids. A name in credentialHeaders, or
	// one this run's own inject rules touch, is reduced to credentialShape regardless.
	headerValues map[string]bool

	// ruleHeaders are the header names this run's inject rules strip or set. They are treated as
	// credential-bearing for logging purposes even if not in credentialHeaders: a rule exists
	// precisely because that header carries the credential krayt is attaching.
	ruleHeaders map[string]bool
}

// newObserver builds the observer for a policy, or nil when observation is off.
func newObserver(p Policy) *observer {
	if !p.LogRequests && len(p.LogHeaderValues) == 0 {
		return nil
	}
	o := &observer{headerValues: map[string]bool{}, ruleHeaders: map[string]bool{}}
	for _, name := range p.LogHeaderValues {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			o.headerValues[name] = true
		}
	}
	for _, r := range p.Inject {
		for _, name := range r.Strip {
			o.ruleHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
		for name := range r.Set {
			o.ruleHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
		for name := range r.SetLiteral {
			o.ruleHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return o
}

// connect logs an allowed CONNECT and which path will handle it. For a tunneled host this is the
// ONLY observation possible — those bytes stay end-to-end encrypted — so it is logged here rather
// than folded into the MITM path's decrypted-request line.
func (o *observer) connect(authority string, intercepted bool) {
	if o == nil {
		return
	}
	via := "tunnel"
	if intercepted {
		via = "mitm"
	}
	log.Printf("krayt-egress-proxy: observe CONNECT %q via=%s", authority, via)
}

// request writes one request observation. host is the ALREADY-APPROVED target (the CONNECT
// authority on the MITM path, the absolute-URL host for a plain forward) — never the guest's inner
// Host header, which is attacker-controlled (§6). injected records whether an injection rule
// matched, so a probe can tell an observed-as-sent request from a rewritten one.
func (o *observer) request(via, host string, r *http.Request, injected bool) {
	if o == nil {
		return
	}
	log.Printf("krayt-egress-proxy: observe %s %s host=%q path=%q%s headers=%s inject=%t%s",
		via, r.Method, host, urlPath(r.URL), queryNamesField(r.URL), headerNames(r.Header), injected,
		o.valuesField(r.Header))
}

// response writes the upstream's reply to an observed request: status code and response header
// NAMES. The status is what makes a 401-then-refresh sequence (§4.6) visible in proxy.log; the body
// is never read here, only forwarded.
//
// It also reports the request AS SENT upstream (the `sent=` fragment), which is the only place
// injection can be observed taking effect: observer.request logs what the GUEST sent, before
// strip/set ran. Without this, a run using shape translation shows a placeholder going in
// and a 200 coming back, with the actual translated credential shape invisible — exactly the thing
// a verification run needs to confirm. Credential-bearing headers are reduced to scheme + length
// here as everywhere else, so the injected value itself never appears.
func (o *observer) response(via, host string, resp *http.Response) {
	if o == nil {
		return
	}
	var u *url.URL
	var sent string
	if resp.Request != nil {
		u = resp.Request.URL
		if f := o.valuesField(resp.Request.Header); f != "" {
			sent = " sent" + strings.TrimPrefix(f, " values")
		}
	}
	log.Printf("krayt-egress-proxy: observe %s response host=%q path=%q status=%d headers=%s%s%s",
		via, host, urlPath(u), resp.StatusCode, headerNames(resp.Header), sent, o.valuesField(resp.Header))
}

// valuesField renders the " values=[…]" fragment for the headers an operator named in
// Policy.LogHeaderValues, or "" when none of them are present. A credential-bearing name yields
// credentialShape, never its value.
func (o *observer) valuesField(hdr http.Header) string {
	if len(o.headerValues) == 0 {
		return ""
	}
	fields := make([]string, 0, len(o.headerValues))
	for name := range o.headerValues {
		v := hdr.Get(name)
		if v == "" {
			continue // absent (or genuinely empty) — say nothing rather than assert a negative
		}
		if credentialHeaders[name] || o.ruleHeaders[name] {
			fields = append(fields, name+"="+credentialShape(v))
			continue
		}
		fields = append(fields, name+"="+strconv.Quote(v))
	}
	if len(fields) == 0 {
		return ""
	}
	sort.Strings(fields)
	return " values=[" + strings.Join(fields, " ") + "]"
}

// credentialShape describes a credential-bearing header value WITHOUT reproducing it: the RFC 7235
// auth scheme (a public, standardized token — "Bearer", "Basic", …) and the length of the material
// after it. That is what answers a probe's "the auth header name and its VALUE'S SHAPE only"
// question, and the length is what distinguishes a credential forwarded verbatim from one the
// client exchanged for something else first (compare it against the secrets-file value's own
// length) — a distinction that decides whether host-side shape translation can work at all.
//
// The length of a high-entropy token is a deliberate, narrow disclosure into an artifact that is
// already redacted and already opt-in per run; the bytes themselves never appear.
func credentialShape(v string) string {
	scheme, rest, found := strings.Cut(v, " ")
	if !found || !isSchemeToken(scheme) {
		return fmt.Sprintf("<scheme=none credential_len=%d>", len(v))
	}
	return fmt.Sprintf("<scheme=%q credential_len=%d>", scheme, len(rest))
}

// authSchemes is the IANA-registered HTTP authentication scheme names (RFC 7235 §5.1 and the
// "Hypertext Transfer Protocol (HTTP) Authentication Scheme Registry"), matched case-insensitively.
// This is a fixed, public list, not vendor knowledge — it does not grow to fit any one credential.
var authSchemes = map[string]bool{
	"basic": true, "bearer": true, "digest": true, "hoba": true, "mutual": true,
	"negotiate": true, "oauth": true, "scram-sha-1": true, "scram-sha-256": true,
	"vapid": true, "aws4-hmac-sha256": true, "concealed": true, "dpop": true,
}

// isSchemeToken reports whether s is one of the fixed, recognized auth-scheme names in
// authSchemes. Checking against a closed list — rather than merely "looks token-shaped" — matters
// because a credential's own first word can otherwise look like a plausible scheme (e.g. a
// passphrase-style secret containing a space): printing it verbatim as "the scheme" would defeat
// credentialShape's entire purpose of never reproducing credential bytes. A value that isn't a
// recognized scheme is reported as scheme-less instead.
func isSchemeToken(s string) bool {
	return authSchemes[strings.ToLower(s)]
}

// urlPath is u's path, or "" for a nil URL. Deliberately path-only: a query string can carry a
// token (the §9 case that made proxy.log a redacted artifact in the first place), so values are
// dropped here and only parameter NAMES are reported, via queryNamesField.
func urlPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Path
}

// queryNamesField renders the sorted NAMES of u's query parameters as a " query=[…]" fragment, or
// "" when there is no query — appended to a request line so a probe can see that, say, a beta flag
// rode the URL, without the log ever holding a parameter value.
func queryNamesField(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	q := u.Query() // keys only below; ParseQuery drops malformed pairs, which is fine for a log
	if len(q) == 0 {
		return ""
	}
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, strconv.Quote(name)) // percent-decoded, so quote each one
	}
	sort.Strings(names)
	return " query=[" + strings.Join(names, ",") + "]"
}

// headerNames renders hdr's field names, lowercased and sorted, as "[a,b,c]" — never a value.
func headerNames(hdr http.Header) string {
	names := make([]string, 0, len(hdr))
	for name := range hdr {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	return "[" + strings.Join(names, ",") + "]"
}
