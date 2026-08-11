// Package selfupdate implements the host-side logic behind `krayt upgrade`: resolving a GitHub
// release, downloading the right platform tarball, verifying it against the release's published
// checksums.txt, and atomically swapping it in for the currently-running binary. It talks only to
// the GitHub Releases API and CDN — nothing here is routed through krayt's own guest
// egress-allowlist model (§6.6), which governs sandboxed agent traffic, not krayt's own process.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// githubRepo is the owner/repo this package checks releases against — the single source of
// truth for every GitHub API path built below.
const githubRepo = "418-cloud/krayt"

// apiBaseURL is overridable in tests to point at an httptest.Server instead of the real GitHub
// API.
var apiBaseURL = "https://api.github.com"

// userAgent is sent on every request — GitHub's API returns 403 for requests with no User-Agent.
const userAgent = "krayt-upgrade"

// Release is the subset of the GitHub Releases API response this package needs.
type Release struct {
	TagName string  `json:"tag_name"` // e.g. "v0.6.1" — always "v"-prefixed
	Assets  []Asset `json:"assets"`
}

// Asset is one file attached to a Release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// LatestRelease fetches the repo's latest published release.
func LatestRelease(ctx context.Context, client *http.Client) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBaseURL, githubRepo)
	return getRelease(ctx, client, url, "")
}

// ReleaseByTag fetches the release tagged tag (e.g. "v0.6.1").
func ReleaseByTag(ctx context.Context, client *http.Client, tag string) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", apiBaseURL, githubRepo, tag)
	return getRelease(ctx, client, url, tag)
}

func getRelease(ctx context.Context, client *http.Client, url, tag string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("fetch release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		if tag != "" {
			return Release{}, fmt.Errorf("release %s not found", tag)
		}
		return Release{}, fmt.Errorf("no releases found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Release{}, fmt.Errorf("github api returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("decode release JSON: %w", err)
	}
	return rel, nil
}

// AssetName returns the release asset filename for the given platform and tag. Only the three
// combinations actually published by .github/workflows/release-please.yml's build matrix
// (darwin/arm64, darwin/amd64, linux/amd64) are supported — kept as an explicit switch here since
// it can't share code with the YAML build matrix and the two must be kept in sync by hand. There
// is no linux/arm64 build; see README.md's "Prebuilt binaries" paragraph for why.
func AssetName(goos, goarch, tag string) (string, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64", "darwin/amd64", "linux/amd64":
		return fmt.Sprintf("krayt_%s_%s_%s.tar.gz", tag, goos, goarch), nil
	default:
		return "", fmt.Errorf(
			"krayt upgrade does not support %s/%s — see README.md's \"Prebuilt binaries\" paragraph for supported platforms",
			goos, goarch)
	}
}

// TargetAssetName is AssetName for the platform this binary was built for.
func TargetAssetName(tag string) (string, error) {
	return AssetName(runtime.GOOS, runtime.GOARCH, tag)
}

// FindAsset looks up a release asset by exact name.
func FindAsset(rel Release, name string) (Asset, error) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no asset named %s", rel.TagName, name)
}

// ParseChecksums parses GNU coreutils sha256sum text-mode output (`<64-hex digest>  <filename>`,
// one line per file) into a map of filename -> lowercase hex digest. An optional leading `*` on
// the filename (binary-mode marker) is tolerated and stripped, though the real generated
// checksums.txt never has one.
func ParseChecksums(data []byte) (map[string]string, error) {
	out := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums.txt: malformed line %d: %q", line, text)
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != 64 {
			return nil, fmt.Errorf("checksums.txt: malformed digest on line %d: %q", line, text)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("checksums.txt: malformed digest on line %d: %q", line, text)
		}
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read checksums.txt: %w", err)
	}
	return out, nil
}

// DownloadAndVerify streams url's body into a temp file inside destDir (the eventual install
// target's own directory, so the caller's later rename is same-filesystem/atomic), verifying its
// SHA-256 against wantSHA256Hex as it goes. On any error, including a checksum mismatch, the temp
// file is removed before returning — no unverified bytes are ever left behind.
func DownloadAndVerify(ctx context.Context, client *http.Client, url, wantSHA256Hex, destDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(destDir, ".krayt-upgrade-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file in %s: %w", destDir, err)
	}
	tmpPath := tmp.Name()

	hasher := sha256.New()
	_, copyErr := io.Copy(tmp, io.TeeReader(resp.Body, hasher))
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temp file %s: %w", tmpPath, closeErr)
	}

	gotSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotSHA256, wantSHA256Hex) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("checksum mismatch for %s: want %s, got %s", url, wantSHA256Hex, gotSHA256)
	}
	return tmpPath, nil
}

// ExtractBinary gunzips + untars tarGzPath, which must contain exactly one regular-file entry
// named "krayt" (the workflow that produces it always tars a single file named "krayt"), and
// writes that entry's bytes to a new 0755 temp file in destDir. It fails closed — erroring
// rather than guessing — if the archive has zero entries, more than one entry, its one entry
// isn't a regular file, or that entry isn't named "krayt".
func ExtractBinary(tarGzPath, destDir string) (string, error) {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("open %s as gzip: %w", tarGzPath, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var hdr *tar.Header
	var content []byte
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar %s: %w", tarGzPath, err)
		}
		entries++
		if entries > 1 {
			return "", fmt.Errorf("%s: expected exactly one entry, found more than one", tarGzPath)
		}
		hdr = h
		content, err = io.ReadAll(tr)
		if err != nil {
			return "", fmt.Errorf("read entry %s from %s: %w", h.Name, tarGzPath, err)
		}
	}
	if entries == 0 {
		return "", fmt.Errorf("%s: archive is empty", tarGzPath)
	}
	if hdr.Typeflag != tar.TypeReg {
		return "", fmt.Errorf("%s: entry %s is not a regular file", tarGzPath, hdr.Name)
	}
	if name := strings.TrimPrefix(hdr.Name, "./"); name != "krayt" {
		return "", fmt.Errorf("%s: expected entry named %q, found %q", tarGzPath, "krayt", hdr.Name)
	}

	tmp, err := os.CreateTemp(destDir, ".krayt-upgrade-bin-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file in %s: %w", destDir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write extracted binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close extracted binary: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod extracted binary: %w", err)
	}
	return tmpPath, nil
}

// CompareVersions numerically compares two "vX.Y.Z" or "X.Y.Z" version strings, returning -1, 0,
// or 1. It is not a string compare: "0.9.0" vs "0.10.0" would compare backwards lexically.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	trimmed := strings.TrimPrefix(v, "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("invalid version %q: expected 3 dot-separated components", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("invalid version %q: component %q is not numeric", v, p)
		}
		out[i] = n
	}
	return out, nil
}

// Apply installs newBinaryPath over currentPath: it verifies currentPath's directory is
// writable, backs up currentPath to currentPath+".bak" (overwriting any prior backup), then
// atomically renames newBinaryPath into place. Replacing an open, currently-executing file this
// way is well-defined on Unix — the running process keeps its already-mapped inode until it
// exits. Never attempts privilege escalation: an unwritable directory is a plain error.
func Apply(currentPath, newBinaryPath string) (string, error) {
	dir := filepath.Dir(currentPath)

	probe, err := os.CreateTemp(dir, ".krayt-upgrade-probe-*.tmp")
	if err != nil {
		return "", fmt.Errorf(
			"%s is not writable by the current user — re-run with sufficient permissions, or reinstall krayt to a user-writable location: %w",
			dir, err)
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)

	backupPath := currentPath + ".bak"
	if err := copyFile(currentPath, backupPath); err != nil {
		return "", fmt.Errorf("back up %s to %s: %w", currentPath, backupPath, err)
	}

	if err := os.Rename(newBinaryPath, currentPath); err != nil {
		return "", fmt.Errorf("install new binary over %s: %w", currentPath, err)
	}

	return backupPath, nil
}

// copyFile copies src's content and mode to dst, writing through a same-directory temp file and
// renaming it into place so a failed copy never leaves a partial dst behind.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmpPath := dst + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// ResolveCurrentExecutable returns the real, symlink-resolved path to the running binary, so a
// symlinked install (e.g. /usr/local/bin/krayt -> /opt/krayt/bin/krayt) gets its real target
// replaced, leaving the symlink itself intact.
func ResolveCurrentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %s: %w", exe, err)
	}
	return resolved, nil
}
