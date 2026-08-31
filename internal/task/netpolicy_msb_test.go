package task

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNetworkArgsAllowlistGolden(t *testing.T) {
	np := NetworkPolicy{
		Mode:  NetworkAllowlist,
		Allow: []string{"api.anthropic.com", "generativelanguage.googleapis.com"},
	}
	want := []string{
		"--net-default", "deny",
		"--net-rule", "deny@private",
		"--net-rule", "deny@loopback",
		"--net-rule", "deny@link-local",
		"--net-rule", "deny@meta",
		"--net-rule", "deny@multicast",
		"--net-rule", "deny@host",
		"--net-rule", "allow@dns",
		"--net-rule", "allow@api.anthropic.com",
		"--net-rule", "allow@generativelanguage.googleapis.com",
		"--on-secret-violation", "block-and-log",
	}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestNetworkArgsAllowlistEmptyIsDenyAll(t *testing.T) {
	// §6.6: "with none listed it is deny-all" — allowlist mode with no hosts must still resolve
	// DNS (decision 7) but permit nothing else, never fall through to msb's own implicit public
	// allow.
	np := NetworkPolicy{Mode: NetworkAllowlist}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	for _, tok := range got {
		if strings.HasPrefix(tok, "allow@") && tok != "allow@dns" {
			t.Errorf("empty allow list produced an unexpected allow rule: %q in %v", tok, got)
		}
	}
	assertNetDefaultOrNoNet(t, got)
}

func TestNetworkArgsFullGolden(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkFull}
	want := []string{
		"--net-default-egress", "allow", "--net-default-ingress", "deny",
		"--net-rule", "deny@private",
		"--net-rule", "deny@loopback",
		"--net-rule", "deny@link-local",
		"--net-rule", "deny@meta",
		"--net-rule", "deny@multicast",
		"--net-rule", "deny@host",
		"--on-secret-violation", "block-and-log",
	}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestNetworkArgsFullDenyRulesPrecedeAnyAllow(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkFull, Passthrough: nil}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	lastDenyIdx := -1
	firstAllowIdx := -1
	for i, tok := range got {
		if strings.HasPrefix(tok, "deny@") {
			lastDenyIdx = i
		}
		if strings.HasPrefix(tok, "allow@") && firstAllowIdx == -1 {
			firstAllowIdx = i
		}
	}
	if lastDenyIdx == -1 {
		t.Fatal("full mode produced no deny@ rules")
	}
	for _, g := range msbDenyGroups {
		if !contains(got, "deny@"+g) {
			t.Errorf("full mode missing deny@%s: %v", g, got)
		}
	}
	if firstAllowIdx != -1 && firstAllowIdx < lastDenyIdx {
		t.Errorf("an allow@ rule (index %d) precedes a deny@ rule (index %d): %v", firstAllowIdx, lastDenyIdx, got)
	}
}

func TestNetworkArgsNoneGolden(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkNone}
	want := []string{"--no-net", "--on-secret-violation", "block-and-log"}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestNetworkArgsNoneEmitsZeroNetRules(t *testing.T) {
	// The single most important none-mode property: --net none would still attach any supplied
	// rule to NetworkPolicy::none() (msb common.rs:2459-2465), so translating "no allowed hosts"
	// as "no --net-rule flags" would silently hand the sandbox the whole internet were a rule ever
	// present. --no-net plus zero --net-rule is the only safe translation.
	np := NetworkPolicy{Mode: NetworkNone, Allow: []string{"should-be-ignored.example"}}
	got, err := NetworkArgs(np, true)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	for _, tok := range got {
		if tok == "--net-rule" {
			t.Fatalf("none mode emitted a --net-rule flag: %v", got)
		}
	}
	if !contains(got, "--no-net") {
		t.Errorf("none mode missing --no-net: %v", got)
	}
}

// TestNetworkArgsNeverEmptyWithoutDefaultFlag is the load-bearing property test
// (translate-network-policy-to-msb.md, "Done when"): whatever NetworkArgs returns successfully,
// for every input including the zero value, must carry a --net-default*/--no-net flag before any
// --net-rule could appear — that is what makes the "--net-rule alone" trap unreachable through
// this function. An input this function cannot translate must error rather than produce a
// slice that omits the flag.
func TestNetworkArgsNeverEmptyWithoutDefaultFlag(t *testing.T) {
	cases := []struct {
		name       string
		np         NetworkPolicy
		hasSecrets bool
		wantErr    bool
	}{
		{name: "zero value", np: NetworkPolicy{}, wantErr: true},
		{name: "unknown mode", np: NetworkPolicy{Mode: "bogus"}, wantErr: true},
		{name: "allowlist, empty", np: NetworkPolicy{Mode: NetworkAllowlist}},
		{name: "allowlist, with hosts", np: NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"a.example"}}},
		{name: "full", np: NetworkPolicy{Mode: NetworkFull}},
		{name: "none", np: NetworkPolicy{Mode: NetworkNone}},
		{name: "none with secrets", np: NetworkPolicy{Mode: NetworkNone}, hasSecrets: true},
		{
			name: "allowlist with passthrough and secrets",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"a.example"}, Passthrough: []string{"a.example"},
			},
			hasSecrets: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NetworkArgs(tc.np, tc.hasSecrets)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NetworkArgs(%+v) = %v, want error", tc.np, got)
				}
				if len(got) != 0 {
					t.Errorf("NetworkArgs(%+v) returned a non-empty slice alongside an error: %v", tc.np, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NetworkArgs(%+v): %v", tc.np, err)
			}
			assertNetDefaultOrNoNet(t, got)
		})
	}
}

// assertNetDefaultOrNoNet asserts args contains --no-net, or --net-default, or the
// --net-default-egress/--net-default-ingress pair, at an index earlier than any --net-rule token
// — the property that makes a --net-rule-only argv (the msb trap) structurally unreachable.
func assertNetDefaultOrNoNet(t *testing.T, args []string) {
	t.Helper()
	defaultIdx := -1
	for i, tok := range args {
		switch tok {
		case "--no-net", "--net-default", "--net-default-egress", "--net-default-ingress":
			if defaultIdx == -1 || i < defaultIdx {
				defaultIdx = i
			}
		}
	}
	if defaultIdx == -1 {
		t.Fatalf("argv carries no --net-default*/--no-net flag: %v", args)
	}
	for i, tok := range args {
		if tok == "--net-rule" && i < defaultIdx {
			t.Fatalf("a --net-rule token (index %d) precedes the default/no-net flag (index %d): %v", i, defaultIdx, args)
		}
	}
}

func TestNetworkArgsTLSInterceptIffHasSecrets(t *testing.T) {
	for _, hasSecrets := range []bool{false, true} {
		np := NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"a.example"}}
		got, err := NetworkArgs(np, hasSecrets)
		if err != nil {
			t.Fatalf("NetworkArgs: %v", err)
		}
		present := contains(got, "--tls-intercept")
		if present != hasSecrets {
			t.Errorf("hasSecrets=%v: --tls-intercept present=%v, want %v (%v)", hasSecrets, present, hasSecrets, got)
		}
	}
}

func TestNetworkArgsOnSecretViolationAlwaysExplicit(t *testing.T) {
	for _, np := range []NetworkPolicy{
		{Mode: NetworkAllowlist},
		{Mode: NetworkFull},
		{Mode: NetworkNone},
	} {
		for _, hasSecrets := range []bool{false, true} {
			got, err := NetworkArgs(np, hasSecrets)
			if err != nil {
				t.Fatalf("NetworkArgs(%+v, %v): %v", np, hasSecrets, err)
			}
			if !containsPair(got, "--on-secret-violation", "block-and-log") {
				t.Errorf("mode=%s hasSecrets=%v: missing explicit --on-secret-violation block-and-log: %v",
					np.Mode, hasSecrets, got)
			}
		}
	}
}

func TestNetworkArgsPassthroughEmitsTLSBypass(t *testing.T) {
	np := NetworkPolicy{
		Mode:        NetworkAllowlist,
		Allow:       []string{"github.com", "api.example.com"},
		Passthrough: []string{"github.com"},
	}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	if !containsPair(got, "--tls-bypass", "github.com") {
		t.Errorf("missing --tls-bypass github.com: %v", got)
	}
}

// TestNetworkArgsHostsAreOwnArgvElements guards against a later refactor that string-joins
// --net-rule tokens: each rendered rule (containing '@' and, for hosts with a port-bearing IPv6
// literal, ':') must be exactly one slice element, never shell-joined with a comma or a space —
// krayt never goes through a shell, so joining would corrupt the token rather than merely reading
// oddly.
func TestNetworkArgsHostsAreOwnArgvElements(t *testing.T) {
	np := NetworkPolicy{
		Mode:  NetworkAllowlist,
		Allow: []string{"a.example", "b.example"},
	}
	got, err := NetworkArgs(np, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	for _, tok := range got {
		if strings.Contains(tok, ",") {
			t.Errorf("argv element %q contains a comma — looks string-joined", tok)
		}
		if strings.Contains(tok, " ") {
			t.Errorf("argv element %q contains a space — looks string-joined", tok)
		}
	}
	if !containsPair(got, "--net-rule", "allow@a.example") {
		t.Errorf("allow@a.example not its own pair of argv elements: %v", got)
	}
	if !containsPair(got, "--net-rule", "allow@b.example") {
		t.Errorf("allow@b.example not its own pair of argv elements: %v", got)
	}
}

// TestNetworkArgsThisRepoConfig pins the exact argv this repo's own tracked krayt.yaml would
// produce under msb's allowlist translation, loaded from the real file rather than a hand-copied
// list, so the golden test cannot drift silently out of sync with it.
func TestNetworkArgsThisRepoConfig(t *testing.T) {
	cfgPath := filepath.Join("..", "..", "krayt.yaml")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("this repo's krayt.yaml: %v", err)
	}
	if cfg.Network.Mode != string(NetworkAllowlist) {
		t.Fatalf("this repo's krayt.yaml network.mode = %q, want %q", cfg.Network.Mode, NetworkAllowlist)
	}
	if len(cfg.Network.Allow) == 0 {
		t.Fatal("this repo's krayt.yaml has an empty network.allow — test would be vacuous")
	}

	np := NetworkPolicy{Mode: NetworkAllowlist, Allow: cfg.Network.Allow}
	got, err := NetworkArgs(np, true) // this repo's config injects a credential (§6.6.1)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}

	want := []string{"--net-default", "deny"}
	for _, g := range msbDenyGroups {
		want = append(want, "--net-rule", "deny@"+g)
	}
	want = append(want, "--net-rule", "allow@dns")
	for _, h := range cfg.Network.Allow {
		want = append(want, "--net-rule", "allow@"+h)
	}
	want = append(want, "--tls-intercept", "--on-secret-violation", "block-and-log")

	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch for this repo's krayt.yaml:\n got  %#v\n want %#v", got, want)
	}
}

func TestValidateNetworkPolicyForMsbRejectsMitm(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true}

	// The deferred-activation property: the existing validator still accepts mitm today (the
	// vfkit/Firecracker path requires it to inject anything), while the msb-era validator rejects
	// it outright. Both must hold at once, proving the activation is deliberately deferred rather
	// than assumed.
	if err := ValidateNetworkPolicy(np, nil); err != nil {
		t.Fatalf("ValidateNetworkPolicy unexpectedly rejected mitm: %v", err)
	}
	err := ValidateNetworkPolicyForMsb(np, nil)
	if err == nil {
		t.Fatal("ValidateNetworkPolicyForMsb accepted network.mitm")
	}
	if !strings.Contains(err.Error(), "mitm") {
		t.Errorf("error %q does not name mitm", err)
	}
}

func TestValidateNetworkPolicyForMsbAcceptsOtherwiseValidPolicy(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}}
	if err := ValidateNetworkPolicyForMsb(np, nil); err != nil {
		t.Fatalf("ValidateNetworkPolicyForMsb: %v", err)
	}
}

func TestValidateNetworkPolicyForMsbStillEnforcesInjectRequiresMitm(t *testing.T) {
	// mitm is now unconditionally rejected, so a leftover inject rule (out of scope for this task
	// to remove — hand-secrets-to-msb.md's job) still fails, just via the same "inject requires
	// mitm: true" path ValidateNetworkPolicy already enforces, since mitm can never be true here.
	np := NetworkPolicy{
		Mode: NetworkAllowlist, Allow: []string{"api.github.com"},
		Inject: []InjectRule{{Host: "api.github.com", Set: map[string]string{"authorization": "GH_TOKEN"}}},
	}
	err := ValidateNetworkPolicyForMsb(np, map[string]bool{"GH_TOKEN": true})
	if err == nil {
		t.Fatal("expected an error for an inject rule with no mitm")
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func containsPair(ss []string, a, b string) bool {
	for i := 0; i+1 < len(ss); i++ {
		if ss[i] == a && ss[i+1] == b {
			return true
		}
	}
	return false
}
