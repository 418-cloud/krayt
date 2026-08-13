package proxy

import (
	"fmt"
	"net"
	"os"
)

// ListenerFromFD adopts an inherited file descriptor (passed via exec.Cmd.ExtraFiles, §4 of
// move-egress-proxy-to-host.md) as a net.Listener. net.FileListener dup's the fd internally,
// so the *os.File wrapper is closed immediately after — the returned Listener owns its own
// independent descriptor from that point on.
//
// Used by the `krayt __egress-proxy` hidden subcommand (internal/cli) to adopt its fd-3
// listener, and by internal/orchestrator's tests, which re-exec the real proxy.Serve loop as
// a genuine child process rather than mocking it.
func ListenerFromFD(fd uintptr) (net.Listener, error) {
	f := os.NewFile(fd, "egress")
	if f == nil {
		return nil, fmt.Errorf("proxy: fd %d is not open (must be spawned with a listener there, not run directly)", fd)
	}
	lis, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("proxy: fd %d is not a listener: %w", fd, err)
	}
	return lis, nil
}
