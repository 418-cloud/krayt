package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/selfupdate"
)

// execPath and httpClient are testability seams: tests override execPath to point at a temp
// file standing in for "the installed krayt" (never the real go test binary), and httpClient to
// one bound to an httptest.Server.
var (
	execPath   = selfupdate.ResolveCurrentExecutable
	httpClient = &http.Client{} // zero-value: uses http.DefaultTransport, honors HTTP_PROXY/HTTPS_PROXY/NO_PROXY
)

// upgradeFlags holds `krayt upgrade`'s flag set (§13).
type upgradeFlags struct {
	version string
	yes     bool
	check   bool
}

// newUpgradeCmd builds the `krayt upgrade` command: it finds the latest (or a pinned) GitHub
// release, downloads the platform tarball, verifies it against the release's checksums.txt, and
// atomically replaces the currently-running binary. On-demand only — no other command talks to
// GitHub, and this has nothing to do with the guest egress-allowlist model (§6.6), which governs
// sandboxed agent traffic, not krayt's own process.
func newUpgradeCmd() *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update krayt in place from a GitHub release",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runUpgrade(cmd, &f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.version, "version", "", "target a specific release (vX.Y.Z or X.Y.Z) instead of latest; pins, downgrades, or reinstalls")
	fl.BoolVarP(&f.yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&f.check, "check", false, "report current/latest version and whether an upgrade is available; never downloads or writes anything")
	return cmd
}

func runUpgrade(cmd *cobra.Command, f *upgradeFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	rel, err := resolveTargetRelease(ctx, f.version)
	if err != nil {
		return err
	}

	targetVersion := strings.TrimPrefix(rel.TagName, "v")
	cmp, err := selfupdate.CompareVersions(Version, targetVersion)
	if err != nil {
		return err
	}

	if f.check {
		return printCheckStatus(out, targetVersion, cmp)
	}

	if f.version == "" && cmp == 0 {
		_, err := fmt.Fprintf(out, "krayt is already at the latest version (v%s).\n", Version)
		return err
	}

	assetName, err := selfupdate.TargetAssetName(rel.TagName)
	if err != nil {
		return err
	}

	path, err := execPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	// cmp = CompareVersions(Version, targetVersion): positive means the current version is
	// numerically greater than the target, i.e. installing targetVersion is a downgrade.
	verb := "upgrade"
	if cmp > 0 {
		verb = "downgrade"
	}
	if _, err := fmt.Fprintf(out, "krayt %s → %s (%s)\ninstall path: %s\n", Version, targetVersion, verb, path); err != nil {
		return err
	}

	if !f.yes {
		proceed, err := confirmUpgrade(cmd, out)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
	}

	checksumsAsset, err := selfupdate.FindAsset(rel, "checksums.txt")
	if err != nil {
		return err
	}
	checksums, err := fetchChecksums(ctx, checksumsAsset.BrowserDownloadURL)
	if err != nil {
		return err
	}

	tarballAsset, err := selfupdate.FindAsset(rel, assetName)
	if err != nil {
		return err
	}
	wantSHA256, ok := checksums[assetName]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", assetName)
	}

	tarballTmp, err := selfupdate.DownloadAndVerify(ctx, httpClient, tarballAsset.BrowserDownloadURL, wantSHA256, dir)
	if tarballTmp != "" {
		defer removeIfExists(tarballTmp)
	}
	if err != nil {
		return err
	}

	binaryTmp, err := selfupdate.ExtractBinary(tarballTmp, dir)
	if binaryTmp != "" {
		defer removeIfExists(binaryTmp)
	}
	if err != nil {
		return err
	}

	backupPath, err := selfupdate.Apply(path, binaryTmp)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "krayt upgraded to %s.\nprevious binary backed up at %s (restore with: cp %s %s)\n",
		targetVersion, backupPath, backupPath, path); err != nil {
		return err
	}

	verifyCmd := exec.CommandContext(ctx, path, "version")
	verifyCmd.Stdout = out
	verifyCmd.Stderr = out
	if err := verifyCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(out, "warning: running %s version to confirm the upgrade failed: %v\n", path, err)
	}

	return nil
}

// resolveTargetRelease fetches the release to install: a pinned tag if version is set
// (normalized to always have a leading "v"), otherwise the latest release.
func resolveTargetRelease(ctx context.Context, version string) (selfupdate.Release, error) {
	if version == "" {
		return selfupdate.LatestRelease(ctx, httpClient)
	}
	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return selfupdate.ReleaseByTag(ctx, httpClient, tag)
}

// printCheckStatus implements --check: report status only, never mutate anything.
func printCheckStatus(out io.Writer, targetVersion string, cmp int) error {
	var status string
	switch {
	case cmp == 0:
		status = "up to date"
	case cmp < 0:
		status = "upgrade available"
	default:
		status = "current version is newer (downgrade)"
	}
	_, err := fmt.Fprintf(out, "current: v%s   target: v%s   %s\n", Version, targetVersion, status)
	return err
}

// confirmUpgrade prints the confirmation prompt and reads one line from stdin. It returns
// (true, nil) only for an explicit "y"/"yes" answer. Empty/closed stdin (io.EOF with nothing
// read — the non-interactive/CI case) declines safely instead of hanging.
func confirmUpgrade(cmd *cobra.Command, out io.Writer) (bool, error) {
	if _, err := fmt.Fprint(out, "Upgrade? [y/N] "); err != nil {
		return false, err
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			_, ferr := fmt.Fprintln(out, "No input available — pass --yes to upgrade non-interactively.")
			return false, ferr
		}
		if !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "y" || answer == "yes" {
		return true, nil
	}
	_, ferr := fmt.Fprintln(out, "Aborted.")
	return false, ferr
}

// fetchChecksums downloads and parses the release's checksums.txt. It's small (one line per
// tarball) so it's fetched as a plain body read rather than through DownloadAndVerify, which is
// sized for the multi-MB tarball.
func fetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download checksums.txt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download checksums.txt: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read checksums.txt: %w", err)
	}
	return selfupdate.ParseChecksums(body)
}

func removeIfExists(path string) {
	_ = os.Remove(path)
}
