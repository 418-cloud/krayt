package sandbox

import "fmt"

// HostCID is the vsock CID msb's local backend exposes the host at from inside a sandbox
// (dial-ask-channel-over-vsock.md) — the guest dials AF_VSOCK to this CID to reach the
// host-side ask bridge (internal/askbridge) msb's `--vsock` route bridges to.
const HostCID uint32 = 2

// AskPort is the fixed vsock port `krayt-ask` (and its --mcp front-end) dial to reach the
// host-side ask_human bridge under msb (dial-ask-channel-over-vsock.md, §6.13): msb's
// `--vsock HOST_PATH:PORT` exposes a host unix socket at HostCID on this port. Moved here from
// the pre-msb provider package (deleted at the run-tasks-on-microsandbox.md cut-over), since msb never
// implemented the Provider interface that constant lived next to — this package is the only one
// left that has any business enumerating krayt's own fixed vsock ports. Must never equal msb's
// own reserved port (123) or the invalid values 0/math.MaxUint32 — see ports_test.go.
const AskPort uint32 = 1026

// AskSocketEnv is the KRAYT_ASK_SOCKET value a --on-question=wait run sets in the container:
// krayt-ask dials this vsock address directly (dial-ask-channel-over-vsock.md) — no in-guest
// forwarder, no guest daemon.
var AskSocketEnv = fmt.Sprintf("vsock://%d:%d", HostCID, AskPort)
