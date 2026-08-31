package task

import "fmt"

// msbDenyGroups are the msb destination groups that must be denied in every mode, including
// `full` (translate-network-policy-to-msb.md decision 4; docs/adr-microsandbox-sandbox-layer.md
// "Default posture: what a bare sandbox gets"). This is an existing krayt property, not a new
// one: move-egress-proxy-to-host.md deleted the old `full`-mode private-range carve-out on
// purpose, because once the dialer left the guest an SSRF to a private address means something
// much worse. The order is fixed so NetworkArgs is deterministic byte-for-byte across calls —
// that determinism is what makes the golden tests in netpolicy_msb_test.go meaningful.
var msbDenyGroups = []string{"private", "loopback", "link-local", "meta", "multicast", "host"}

// NetworkArgs renders a fully explicit msb network policy for np: a default action, an explicit
// DNS decision where one is needed, explicit denies for the private destination groups ordered
// before any allow, and the TLS interception/bypass flags — never anything relying on msb's own
// default. It never returns a slice missing a `--net-default*`/`--no-net` flag: an np.Mode this
// function does not recognize (including the zero value) is a translation error, not a policy
// that silently falls through to msb's own implicit `allow@public` (ADR "Default posture: what a
// bare sandbox gets"; msb's own CLI branches egress default handling on exactly this flag family
// — common.rs:2088-2104 — so anything else it treats as "no policy supplied" and grants the whole
// public internet on top of whatever krayt did pass).
//
// Callers MUST run ValidateNetworkPolicyForMsb (or equivalent) first: this function does not
// itself reject np.MITM — that is the validator's job, kept separate so this stays a pure
// translation with no dependency on secretKeys. It also does not consume np.Inject: under msb
// there is no per-host header vocabulary to translate (hand-secrets-to-msb.md owns the
// secret-delivery flags this task deliberately leaves alone).
//
// hasSecrets controls only whether --tls-intercept is emitted. It is not load-bearing: msb turns
// on interception for a sandbox the moment any --secret is declared, regardless of this flag
// (SandboxBuilder::secret_entry, sdk/rust/lib/sandbox/builder.rs:834-843, confirmed on hardware by
// hack/msb-probes/p3-secret-tls-intercept.sh). Emitting it anyway pins that behavior explicitly
// against a beta tool changing an undocumented builder side effect later (decision 5).
func NetworkArgs(np NetworkPolicy, hasSecrets bool) ([]string, error) {
	var args []string

	switch np.Mode {
	case NetworkNone:
		// --no-net, and nothing else from this switch: build_network_policy still attaches any
		// supplied --net-rule to NetworkPolicy::none() (msb common.rs:2459-2465), so even one
		// stray rule would punch a hole through the mode that is supposed to mean "no network at
		// all". --net none is deliberately not used here for the same reason.
		args = append(args, "--no-net")

	case NetworkFull:
		// Explicit both ways (decision 6): egress allow is the opt-in `full` grants, ingress deny
		// closes msb's own default-allow ingress posture before krayt ever publishes a port.
		args = append(args, "--net-default-egress", "allow", "--net-default-ingress", "deny")
		args = append(args, denyGroupArgs()...)
		// `full`'s allow must not mean "and also the host's LAN" — the deny-group rules above
		// stand even though the default action already allows everything else.

	case NetworkAllowlist:
		args = append(args, "--net-default", "deny")
		args = append(args, denyGroupArgs()...)
		// The guest gains a real, policed network interface under msb (unlike the pre-msb design,
		// where allowlist/none left the guest with no usable network at all) — without this rule
		// nothing resolves (decision 7).
		args = append(args, "--net-rule", "allow@dns")
		for _, h := range np.Allow {
			args = append(args, "--net-rule", "allow@"+h)
		}

	default:
		// Includes the zero value: an unset Mode is "no policy computed", which is a pre-flight
		// error under this design, never a valid state that could fall through to a permissive
		// default (ADR "Default posture: what a bare sandbox gets", closing paragraph).
		return nil, fmt.Errorf("network: cannot render msb args for mode %q", np.Mode)
	}

	for _, h := range np.Passthrough {
		args = append(args, "--tls-bypass", h)
	}
	if hasSecrets {
		args = append(args, "--tls-intercept")
	}
	// Set explicitly rather than inherited, in every mode, regardless of hasSecrets.
	args = append(args, "--on-secret-violation", "block-and-log")

	return args, nil
}

// denyGroupArgs renders one "--net-rule" "deny@<group>" pair per msbDenyGroups entry, in order.
func denyGroupArgs() []string {
	args := make([]string, 0, len(msbDenyGroups)*2)
	for _, g := range msbDenyGroups {
		args = append(args, "--net-rule", "deny@"+g)
	}
	return args
}

// ValidateNetworkPolicyForMsb is the msb-era pre-flight check (translate-network-policy-to-msb.md
// decision 2): it rejects network.mitm outright, naming the key and what replaces it, instead of
// silently dropping a field whose presence means its author was reasoning about interception.
// Under msb there is no such thing as a secret without MITM — declaring any secret enables TLS
// interception automatically (NetworkArgs, decision 5) — so the key has no meaning to translate
// and must not be quietly ignored.
//
// This deliberately does NOT replace ValidateNetworkPolicy, and must not be called in its place
// yet: the vfkit/Firecracker path is still the only path that executes a run, and it requires
// network.mitm: true to inject anything at all — including this repo's own krayt.yaml. Rejecting
// mitm there would break every run on that path. run-tasks-on-microsandbox.md is the task that
// swaps the call site, in the same change that deletes the vfkit/Firecracker path.
//
// Every other check is unchanged by msb, so this delegates to ValidateNetworkPolicy for the rest
// (host shapes, passthrough/allow subset rules, and — until hand-secrets-to-msb.md narrows it —
// the existing inject/mitm-pairing check, which still fires correctly here: mitm is always false
// by the time that check runs, since a true value already returned above).
func ValidateNetworkPolicyForMsb(np NetworkPolicy, secretKeys map[string]bool) error {
	if np.MITM {
		return fmt.Errorf("network: mitm is not a valid key under msb — TLS interception is " +
			"enabled automatically the moment any secret is declared, so there is nothing left " +
			"for this key to opt into; remove network.mitm")
	}
	return ValidateNetworkPolicy(np, secretKeys)
}
