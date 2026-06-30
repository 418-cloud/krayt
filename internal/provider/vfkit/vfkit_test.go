//go:build darwin

package vfkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crc-org/vfkit/pkg/rest/define"

	"github.com/418-cloud/krayt/internal/provider"
)

func TestCloneFileCoW(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.img")
	dst := filepath.Join(dir, "clone.img")
	want := []byte("base-rootfs-contents")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cloneFile(src, dst); err != nil {
		t.Fatalf("cloneFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("clone contents = %q, want %q", got, want)
	}

	// Writing to the clone must not affect the base (copy-on-write isolation).
	if err := os.WriteFile(dst, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(base) != string(want) {
		t.Fatalf("base mutated after writing clone: got %q, want %q", base, want)
	}
}

func TestBuildConfigDevices(t *testing.T) {
	spec := provider.VMSpec{
		ID:        "run_x",
		Kernel:    "/img/vmlinuz",
		Initrd:    "/img/initrd",
		Cmdline:   "console=hvc0",
		RootFS:    "/img/rootfs.img",
		CPUs:      2,
		MemoryMiB: 4096,
	}
	clone := "/run/run_x/rootfs.img"
	scratch := "/run/run_x/scratch.img"
	ctrlSock := "/run/run_x/control.sock"

	cfg, err := buildConfig(spec, clone, scratch, ctrlSock)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	args, err := cfg.ToCmdLine()
	if err != nil {
		t.Fatalf("ToCmdLine: %v", err)
	}
	line := strings.Join(args, " ")

	for _, want := range []string{
		spec.Kernel, spec.Initrd, // Linux bootloader
		clone,          // virtio-blk uses the CoW clone, not the base image
		scratch,        // second virtio-blk: the per-run scratch disk (/dev/vdb)
		ctrlSock,       // vsock bridged to the host control socket
		"virtio-vsock", // control channel device
		"virtio-net",   // NAT NIC
		"port=1024",    // fixed control port (provider.ControlPort)
	} {
		if !strings.Contains(line, want) {
			t.Errorf("cmdline missing %q\n  got: %s", want, line)
		}
	}
	// The base image must never be passed directly to vfkit.
	if strings.Contains(line, spec.RootFS) {
		t.Errorf("cmdline references base rootfs %q directly; should use the clone", spec.RootFS)
	}
}

func TestCreateSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scratch.img")
	const size = uint64(20) << 30 // 20 GiB
	if err := createSparse(path, size); err != nil {
		t.Fatalf("createSparse: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(fi.Size()) != size {
		t.Errorf("logical size = %d, want %d", fi.Size(), size)
	}
	// It must be sparse: blocks actually allocated should be far below the logical size,
	// or a 20 GiB file per run would be ruinous. st_blocks is in 512-byte units.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		allocated := uint64(st.Blocks) * 512
		if allocated > 16<<20 { // generous: a fresh empty file allocates ~nothing
			t.Errorf("scratch file not sparse: %d bytes allocated for a %d-byte file", allocated, size)
		}
	}
}

// roundTripFunc lets a test stand in for vfkit's REST server without binding a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRESTClientStateRoundTrip(t *testing.T) {
	var gotMethod, gotPath, gotState string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotMethod, gotPath = r.Method, r.URL.Path
		var body string
		switch r.Method {
		case http.MethodPost:
			var vs define.VMState
			if err := json.NewDecoder(r.Body).Decode(&vs); err != nil {
				return nil, err
			}
			gotState = vs.State
		case http.MethodGet:
			b, _ := json.Marshal(define.VMState{State: "VirtualMachineStateRunning"})
			body = string(b)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	c := newRESTClient("/unused.sock")
	c.http = &http.Client{Transport: rt}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/vm/state" {
		t.Errorf("stop hit %s %s, want POST /vm/state", gotMethod, gotPath)
	}
	if gotState != string(define.Stop) {
		t.Errorf("server received state %q, want %q", gotState, define.Stop)
	}

	state, err := c.state(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/vm/state" {
		t.Errorf("state hit %s %s, want GET /vm/state", gotMethod, gotPath)
	}
	if state != "VirtualMachineStateRunning" {
		t.Errorf("state = %q, want running", state)
	}
}
