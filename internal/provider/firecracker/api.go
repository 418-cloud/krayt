//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient talks to a running firecracker's REST API, which it serves over a host unix
// socket (`--api-sock …`). Firecracker's API is small and stable, and the handful of
// endpoints krayt needs are hand-rolled here rather than pulled from firecracker-go-sdk:
// the same subprocess + REST-over-unix-socket idiom the vfkit provider already uses, with no
// new module dependencies (which also keeps the guest image's vendorHash unchanged).
//
// Verified against the Firecracker v1.16.1 API spec (firecracker_spec-v1.16.1.yaml). Every
// endpoint below answers 204 No Content on success and carries a {"fault_message": …} body
// on failure.
type apiClient struct {
	sock string
	http *http.Client
}

func newAPIClient(sock string) *apiClient {
	return &apiClient{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
			Timeout: 10 * time.Second,
		},
	}
}

// waitReady blocks until firecracker has bound its API socket and answers, or the deadline
// passes. Firecracker creates the socket a few milliseconds after exec, so configuration
// PUTs would otherwise race the process starting up.
func (c *apiClient) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://firecracker/", nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("api socket %s not ready after %s: %w", c.sock, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *apiClient) setMachineConfig(ctx context.Context, cpus int, memMiB uint64) error {
	return c.put(ctx, "/machine-config", map[string]any{
		"vcpu_count":   cpus,
		"mem_size_mib": memMiB,
	})
}

// setBootSource points firecracker at the kernel + initrd and fixes the guest command line.
// On x86_64 kernel_image_path must be an *uncompressed ELF* — firecracker cannot boot a
// bzImage (see images/flake.nix, which strips the ELF vmlinux out of the kernel's dev output
// for exactly this reason).
func (c *apiClient) setBootSource(ctx context.Context, kernel, initrd, cmdline string) error {
	body := map[string]any{
		"kernel_image_path": kernel,
		"boot_args":         cmdline,
	}
	if initrd != "" {
		body["initrd_path"] = initrd
	}
	return c.put(ctx, "/boot-source", body)
}

// setDrive attaches a raw disk image. Firecracker takes raw block devices only — no qcow2,
// no backing files — which is why the CoW clone in clone.go has to be a real reflink or a
// real copy.
func (c *apiClient) setDrive(ctx context.Context, id, path string, root, readOnly bool) error {
	return c.put(ctx, "/drives/"+id, map[string]any{
		"drive_id":       id,
		"path_on_host":   path,
		"is_root_device": root,
		"is_read_only":   readOnly,
	})
}

func (c *apiClient) setNetworkInterface(ctx context.Context, id, hostDev, mac string) error {
	return c.put(ctx, "/network-interfaces/"+id, map[string]any{
		"iface_id":      id,
		"host_dev_name": hostDev,
		"guest_mac":     mac,
	})
}

// setVsock attaches the virtio-vsock device. uds is the host-side AF_UNIX socket firecracker
// will listen on: host→guest connections dial it and send the CONNECT handshake (§6.12,
// vsock.go). cid is the guest's context ID.
func (c *apiClient) setVsock(ctx context.Context, cid uint32, uds string) error {
	return c.put(ctx, "/vsock", map[string]any{
		"guest_cid": cid,
		"uds_path":  uds,
	})
}

func (c *apiClient) instanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]any{"action_type": "InstanceStart"})
}

// sendCtrlAltDel is the closest thing firecracker has to a power button: systemd in the guest
// turns it into an orderly shutdown, and firecracker exits when the guest resets.
func (c *apiClient) sendCtrlAltDel(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]any{"action_type": "SendCtrlAltDel"})
}

func (c *apiClient) put(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker api: PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	// Firecracker reports configuration errors as {"fault_message": "..."} — surface it
	// verbatim, since it is by far the most useful diagnostic for a bad VM config.
	raw, _ := io.ReadAll(resp.Body)
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	if json.Unmarshal(raw, &fault) == nil && fault.FaultMessage != "" {
		return fmt.Errorf("firecracker api: PUT %s: %s", path, fault.FaultMessage)
	}
	return fmt.Errorf("firecracker api: PUT %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(raw))
}
