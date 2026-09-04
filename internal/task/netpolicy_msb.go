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
		args = append(args, allowDNSArgs()...)
		args = append(args, denyGroupArgs()...)
		// `full`'s allow must not mean "and also the host's LAN" — the deny-group rules above
		// stand even though the default action already allows everything else.

	case NetworkAllowlist:
		args = append(args, "--net-default", "deny")
		// The guest gains a real, policed network interface under msb (unlike the pre-msb design,
		// where allowlist/none left the guest with no usable network at all) — without this rule
		// nothing resolves (decision 7).
		args = append(args, allowDNSArgs()...)
		args = append(args, denyGroupArgs()...)
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

// allowDNSArgs renders the `allow@dns` rule, which must be emitted BEFORE denyGroupArgs in every
// mode that has a network at all. msb evaluates rules first-match-wins within a direction, and
// `dns` is not an abstract capability — it is a destination: msb's own help defines it as "the
// semantic `dns` target for gateway UDP/TCP port 53", and that gateway is the guest's end of a
// /30 carved out of --net-ipv4-pool, which defaults to 172.16.0.0/12. So `dns` and `private`
// name overlapping destinations, and whichever rule krayt emits first decides.
//
// Emitting the denies first — the ordering translate-network-policy-to-msb.md's general "denies
// before allows" rule asks for — therefore matched deny@private on the gateway and the guest
// resolved nothing at all: an agent inside an otherwise correct allowlist sandbox failed every
// request with ENOTFOUND before a single packet reached an allowed host. `dns` is the one target
// that has to precede the deny groups, and it is a narrow exception to make: it opens exactly the
// gateway's port 53, not the private groups those denies exist to close.
func allowDNSArgs() []string {
	return []string{"--net-rule", "allow@dns"}
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
// decision 2, extended by hand-secrets-to-msb.md decision 4 and the "every secret must be
// network-scoped" rule): it rejects network.mitm outright, naming the key and what replaces it,
// instead of silently dropping a field whose presence means its author was reasoning about
// interception. Under msb there is no such thing as a secret without MITM — declaring any secret
// enables TLS interception automatically (NetworkArgs, decision 5) — so the key has no meaning to
// translate and must not be quietly ignored.
//
// inject is network.inject's RAW config entries (task.Config.Network.Inject), not
// NetworkPolicy.Inject — under msb the per-entry schema is key+hosts (SecretSpecsFromConfig), not
// host/strip/set/set_prefix/set_literal, so np.Inject (the pre-msb domain shape) must be empty;
// populating it under msb is itself a caller bug this function refuses rather than misinterprets.
//
// This deliberately does NOT replace ValidateNetworkPolicy, and must not be called in its place
// yet: the vfkit/Firecracker path is still the only path that executes a run, and it requires
// network.mitm: true to inject anything at all — including this repo's own krayt.yaml. Rejecting
// mitm there would break every run on that path. run-tasks-on-microsandbox.md is the task that
// swaps the call site, in the same change that deletes the vfkit/Firecracker path.
//
// secretKeys is the set of key NAMES present in the task's secrets file (never values), exactly
// as ValidateNetworkPolicy takes it; pass nil when there is no secrets file. Every key in
// secretKeys must have a matching inject entry (hand-secrets-to-msb.md, "The gap the ADR does not
// name: non-network secrets") — under msb a secret with nowhere to be scoped can never be
// delivered at all, so leaving one unscoped is a pre-flight error, not a silently-dropped
// capability. Every inject entry must in turn name a key that actually exists in secretKeys, the
// same typo protection ValidateNetworkPolicy already gives the pre-msb shape.
func ValidateNetworkPolicyForMsb(np NetworkPolicy, secretKeys map[string]bool, inject []ConfigInjectRule) error {
	if np.MITM {
		return fmt.Errorf("network: mitm is not a valid key under msb — TLS interception is " +
			"enabled automatically the moment any secret is declared, so there is nothing left " +
			"for this key to opt into; remove network.mitm")
	}
	if len(np.Inject) > 0 {
		return fmt.Errorf("network: NetworkPolicy.Inject (the pre-msb host/strip/set shape) must " +
			"not be populated for an msb run — msb secret scoping is SecretSpecsFromConfig's job, " +
			"not InjectRulesFromConfig's; pass network.inject's raw config entries as this " +
			"function's inject parameter instead")
	}

	specs, err := SecretSpecsFromConfig(inject)
	if err != nil {
		return err
	}
	if secretKeys == nil && len(specs) > 0 {
		return fmt.Errorf("network: inject declares %d key(s) but there is no secrets file — "+
			"inject scopes secrets-file keys to hosts, so it is meaningless without one; remove "+
			"network.inject or add a secrets file", len(specs))
	}

	allow := lowerSet(np.Allow)
	specKeys := make(map[string]bool, len(specs))
	for _, s := range specs {
		specKeys[s.Key] = true
		for _, h := range s.Hosts {
			if err := validateHostEntry(h); err != nil {
				return fmt.Errorf("network: inject (%s): %w", s.Key, err)
			}
			if np.Mode == NetworkAllowlist && !allow[lower(h)] {
				return fmt.Errorf("network: inject (%s): host %q must also be in allow (mode: allowlist)", s.Key, h)
			}
		}
		if secretKeys != nil && !secretKeys[s.Key] {
			return fmt.Errorf("network: inject names secrets-file key %q, which does not exist", s.Key)
		}
	}
	for k := range secretKeys {
		if !specKeys[k] {
			return fmt.Errorf("network: secrets-file key %q has no network.inject entry — under msb "+
				"every secret must be network-scoped: krayt delivers a secret only as a host-side "+
				"substitution to allowed hosts, never materialized inside the guest; add an inject "+
				"entry naming %q and its allowed hosts, or move the value to env: if it genuinely must "+
				"be readable inside the guest", k, k)
		}
	}

	return ValidateNetworkPolicy(np, secretKeys)
}
