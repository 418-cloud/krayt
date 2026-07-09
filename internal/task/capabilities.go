package task

import (
	"fmt"
	"sort"
	"strings"
)

// Capability handling for the per-task `container.capabilities` opt-in (§8.1, §10).
//
// The guest runner drops ALL Linux capabilities by default (§6.10); a task may re-grant a
// specific few here. This validator normalizes and gate-keeps that list host-side so a bad
// value fails fast at config load — before any VM boots — rather than silently granting nothing
// (a typo) or something dangerous.

// knownCapabilities is the set of Linux capabilities a task may name (matching the kernel /
// runtime-spec capability list). A name outside this set is rejected as a typo rather than
// passed through to the OCI spec, where an unknown string would be silently ignored by runc.
var knownCapabilities = map[string]bool{
	"CAP_AUDIT_CONTROL":      true,
	"CAP_AUDIT_READ":         true,
	"CAP_AUDIT_WRITE":        true,
	"CAP_BLOCK_SUSPEND":      true,
	"CAP_BPF":                true,
	"CAP_CHECKPOINT_RESTORE": true,
	"CAP_CHOWN":              true,
	"CAP_DAC_OVERRIDE":       true,
	"CAP_DAC_READ_SEARCH":    true,
	"CAP_FOWNER":             true,
	"CAP_FSETID":             true,
	"CAP_IPC_LOCK":           true,
	"CAP_IPC_OWNER":          true,
	"CAP_KILL":               true,
	"CAP_LEASE":              true,
	"CAP_LINUX_IMMUTABLE":    true,
	"CAP_MAC_ADMIN":          true,
	"CAP_MAC_OVERRIDE":       true,
	"CAP_MKNOD":              true,
	"CAP_NET_ADMIN":          true,
	"CAP_NET_BIND_SERVICE":   true,
	"CAP_NET_BROADCAST":      true,
	"CAP_NET_RAW":            true,
	"CAP_PERFMON":            true,
	"CAP_SETFCAP":            true,
	"CAP_SETGID":             true,
	"CAP_SETPCAP":            true,
	"CAP_SETUID":             true,
	"CAP_SYS_ADMIN":          true,
	"CAP_SYS_BOOT":           true,
	"CAP_SYS_CHROOT":         true,
	"CAP_SYS_MODULE":         true,
	"CAP_SYS_NICE":           true,
	"CAP_SYS_PACCT":          true,
	"CAP_SYS_PTRACE":         true,
	"CAP_SYS_RAWIO":          true,
	"CAP_SYS_RESOURCE":       true,
	"CAP_SYS_TIME":           true,
	"CAP_SYS_TTY_CONFIG":     true,
	"CAP_SYSLOG":             true,
	"CAP_WAKE_ALARM":         true,
}

// deniedCapabilities are never grantable via the opt-in, even if named explicitly (§10). Each
// either re-opens the egress-allowlist bypass this hardening closes — the setuid class lets a
// process become proxyd's uid to satisfy the `skuid "proxyd"` nftables lock, and NET_ADMIN /
// NET_RAW rewrite or bypass the firewall directly — or is a broad container-escape primitive
// (SYS_ADMIN, SYS_PTRACE, DAC_READ_SEARCH, BPF, SETPCAP). A task that genuinely needs open
// networking uses `network.mode: full` (a deliberate, separately-reviewed opt-in), not a cap.
var deniedCapabilities = map[string]string{
	"CAP_SETUID":          "would let the process setuid() to proxyd and bypass the egress allowlist",
	"CAP_SETGID":          "paired with setuid to reach proxyd's gid; re-opens the egress bypass",
	"CAP_SETPCAP":         "can raise capabilities in the bounding set — a privilege-escalation primitive",
	"CAP_SYS_ADMIN":       "near-root; a broad container-escape primitive",
	"CAP_NET_ADMIN":       "can rewrite the nftables egress lock and defeat the allowlist",
	"CAP_NET_RAW":         "raw sockets bypass the proxy and can spoof/observe traffic",
	"CAP_DAC_READ_SEARCH": "bypasses file-read permission checks (open_by_handle_at escape)",
	"CAP_BPF":             "loads BPF programs — a kernel-attack and traffic-tampering surface",
	"CAP_SYS_PTRACE":      "can attach to and manipulate other processes in the VM",
}

// NormalizeCapabilities uppercases each requested capability, adds the CAP_ prefix if missing
// ("net_bind_service" → "CAP_NET_BIND_SERVICE"), rejects unknown names and any denylisted
// capability, and returns the canonical, de-duplicated, sorted list (§8.1, §10). An empty or nil
// input returns nil — the drop-all default. Sorting keeps the pushed TaskSpec deterministic.
func NormalizeCapabilities(caps []string) ([]string, error) {
	if len(caps) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(caps))
	out := make([]string, 0, len(caps))
	for _, raw := range caps {
		name := strings.ToUpper(strings.TrimSpace(raw))
		if name == "" {
			return nil, fmt.Errorf("empty capability entry")
		}
		if !strings.HasPrefix(name, "CAP_") {
			name = "CAP_" + name
		}
		if reason, denied := deniedCapabilities[name]; denied {
			return nil, fmt.Errorf("capability %s is not grantable: %s (use network.mode: full for open networking)", name, reason)
		}
		if !knownCapabilities[name] {
			return nil, fmt.Errorf("unknown capability %q (expected a CAP_* Linux capability name)", raw)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
