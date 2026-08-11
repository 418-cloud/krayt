package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/selfupdate"
)

// upgradeFixture describes the single GitHub release an upgradeFixtureServer serves, both from
// its "latest" endpoint and from its "tags/<tag>" endpoint.
type upgradeFixture struct {
	tag         string
	content     []byte
	badChecksum bool // serve a deliberately wrong digest for the tarball
}

func newUpgradeFixtureServer(t *testing.T, rf upgradeFixture) *httptest.Server {
	t.Helper()

	// Use the real per-platform asset name where possible so it matches exactly what production
	// code (selfupdate.TargetAssetName, via runtime.GOOS/GOARCH) will look up. On an unsupported
	// build platform (e.g. this sandbox may be linux/arm64, deliberately unsupported — see
	// README.md), fall back to a synthetic name just so the fixture JSON is well-formed; tests
	// whose production code path actually needs asset resolution to succeed skip via
	// requireSupportedPlatform instead of exercising this fallback.
	name, err := selfupdate.AssetName(runtime.GOOS, runtime.GOARCH, rf.tag)
	if err != nil {
		name = fmt.Sprintf("krayt_%s_%s_%s.tar.gz", rf.tag, runtime.GOOS, runtime.GOARCH)
	}
	sum := sha256.Sum256(rf.content)
	digest := hex.EncodeToString(sum[:])
	if rf.badChecksum {
		digest = strings.Repeat("0", 64)
	}

	mux := http.NewServeMux()
	var srv *httptest.Server

	writeRelease := func(w http.ResponseWriter) {
		rel := selfupdate.Release{
			TagName: rf.tag,
			Assets: []selfupdate.Asset{
				{Name: name, BrowserDownloadURL: srv.URL + "/download/" + name},
				{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/download/checksums.txt"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}

	mux.HandleFunc("/repos/418-cloud/krayt/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		writeRelease(w)
	})
	mux.HandleFunc("/repos/418-cloud/krayt/releases/tags/"+rf.tag, func(w http.ResponseWriter, _ *http.Request) {
		writeRelease(w)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", digest, name)
	})
	mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(rf.content)
	})

	srv = httptest.NewServer(mux)
	return srv
}

// rewriteHostTransport forces every outgoing request onto the fixture server, regardless of the
// scheme/host the request was built with — LatestRelease/ReleaseByTag hardcode api.github.com,
// which a test server obviously isn't reachable at.
type rewriteHostTransport struct{ base *url.URL }

func (rt *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.base.Scheme
	clone.URL.Host = rt.base.Host
	clone.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func fixtureHTTPClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fixture server URL: %v", err)
	}
	return &http.Client{Transport: &rewriteHostTransport{base: base}}
}

// setupUpgradeTest wires the execPath/httpClient seams (§3) at a fake "installed" binary and a
// fixture GitHub server, and restores both plus cli.Version on cleanup.
func setupUpgradeTest(t *testing.T, rf upgradeFixture, currentContent []byte) string {
	t.Helper()

	srv := newUpgradeFixtureServer(t, rf)
	t.Cleanup(srv.Close)

	origExecPath, origHTTPClient := execPath, httpClient
	t.Cleanup(func() { execPath, httpClient = origExecPath, origHTTPClient })

	dir := t.TempDir()
	currentPath := filepath.Join(dir, "krayt")
	if err := os.WriteFile(currentPath, currentContent, 0o755); err != nil {
		t.Fatalf("write fake current binary: %v", err)
	}
	execPath = func() (string, error) { return currentPath, nil }
	httpClient = fixtureHTTPClient(t, srv)

	return currentPath
}

// requireSupportedPlatform skips tests that need selfupdate.TargetAssetName to actually resolve
// an asset — i.e. every test whose RunE path proceeds past step 5 (asset-name resolution) —
// since that's tied to the real build platform (runtime.GOOS/GOARCH) by design (§2: not
// parameterized, so table-tested via AssetName directly instead). Real CI runs on ubuntu-latest
// (linux/amd64), a supported platform; this only skips on unsupported dev platforms.
func requireSupportedPlatform(t *testing.T) {
	t.Helper()
	if _, err := selfupdate.TargetAssetName("v0.0.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}
}

func withTestVersion(t *testing.T, v string) {
	t.Helper()
	orig := Version
	Version = v
	t.Cleanup(func() { Version = orig })
}

func runUpgradeCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newUpgradeCmd()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	err := cmd.Execute()
	return out.String(), err
}

func TestUpgradeAlreadyUpToDate(t *testing.T) {
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.0.0", content: []byte("new-content")}, []byte("old-content"))

	out, err := runUpgradeCmd(t, "")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !strings.Contains(out, "already at the latest") {
		t.Errorf("output = %q, want mention of already at the latest", out)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "old-content" {
		t.Errorf("current file changed: got %q", got)
	}
}

func TestUpgradeDeclined(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content")}, []byte("old-content"))

	out, err := runUpgradeCmd(t, "n\n")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !strings.Contains(out, "Aborted") {
		t.Errorf("output = %q, want Aborted", out)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "old-content" {
		t.Errorf("current file changed: got %q", got)
	}
}

func TestUpgradeAccepted(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content")}, []byte("old-content"))

	_, err := runUpgradeCmd(t, "y\n")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "new-content" {
		t.Errorf("current content = %q, want new-content", got)
	}
	backup, err := os.ReadFile(currentPath + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "old-content" {
		t.Errorf("backup content = %q, want old-content", backup)
	}
}

func TestUpgradeYesFlagSkipsPrompt(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content")}, []byte("old-content"))

	_, err := runUpgradeCmd(t, "", "--yes")
	if err != nil {
		t.Fatalf("upgrade --yes: %v", err)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "new-content" {
		t.Errorf("current content = %q, want new-content", got)
	}
}

func TestUpgradeCheckNeverMutates(t *testing.T) {
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content")}, []byte("old-content"))

	out, err := runUpgradeCmd(t, "", "--check")
	if err != nil {
		t.Fatalf("upgrade --check: %v", err)
	}
	if !strings.Contains(out, "1.0.0") || !strings.Contains(out, "1.1.0") {
		t.Errorf("output = %q, want both versions mentioned", out)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "old-content" {
		t.Errorf("current file changed under --check: got %q", got)
	}
}

func TestUpgradeVersionFlagDowngrade(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "2.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.0.0", content: []byte("older-release-content")}, []byte("current-content"))

	out, err := runUpgradeCmd(t, "", "--version", "1.0.0", "--yes")
	if err != nil {
		t.Fatalf("upgrade --version 1.0.0: %v", err)
	}
	if !strings.Contains(out, "downgrade") {
		t.Errorf("output = %q, want mention of downgrade", out)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "older-release-content" {
		t.Errorf("current content = %q, want older-release-content", got)
	}
}

func TestUpgradeNonInteractiveDeclinesSafely(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content")}, []byte("old-content"))

	out, err := runUpgradeCmd(t, "")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !strings.Contains(out, "--yes") {
		t.Errorf("output = %q, want a hint to pass --yes", out)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "old-content" {
		t.Errorf("current file changed: got %q", got)
	}
}

func TestUpgradeChecksumMismatch(t *testing.T) {
	requireSupportedPlatform(t)
	withTestVersion(t, "1.0.0")
	currentPath := setupUpgradeTest(t, upgradeFixture{tag: "v1.1.0", content: []byte("new-content"), badChecksum: true}, []byte("old-content"))

	_, err := runUpgradeCmd(t, "", "--yes")
	if err == nil {
		t.Fatal("upgrade with bad checksum: want error, got nil")
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(got) != "old-content" {
		t.Errorf("current file changed after checksum failure: got %q", got)
	}
}
