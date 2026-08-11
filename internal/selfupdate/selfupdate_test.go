package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildFixtureTarGz builds a single-file tar.gz named "krayt" containing content, mirroring
// exactly what release-please.yml's `tar -C dist -czf ... krayt` produces. It returns the
// archive bytes and content's hex SHA-256 digest.
func buildFixtureTarGz(t *testing.T, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "krayt", Mode: 0o755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	sum := sha256.Sum256(content)
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// newFixtureServer serves a single release tagged tag with one tarball asset (tarballName /
// tarballBytes) plus a matching checksums.txt, all at /download/... URLs on the same server —
// mirroring the real GitHub API + CDN redirect shape closely enough for this package's needs.
func newFixtureServer(t *testing.T, tag, tarballName string, tarballBytes []byte, tarballDigest string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server

	writeRelease := func(w http.ResponseWriter) {
		rel := Release{
			TagName: tag,
			Assets: []Asset{
				{Name: tarballName, BrowserDownloadURL: srv.URL + "/download/" + tarballName},
				{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/download/checksums.txt"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}

	mux.HandleFunc("/repos/418-cloud/krayt/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		writeRelease(w)
	})
	mux.HandleFunc("/repos/418-cloud/krayt/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		reqTag := strings.TrimPrefix(r.URL.Path, "/repos/418-cloud/krayt/releases/tags/")
		if reqTag != tag {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeRelease(w)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", tarballDigest, tarballName)
	})
	mux.HandleFunc("/download/"+tarballName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarballBytes)
	})

	srv = httptest.NewServer(mux)
	return srv
}

func withFixtureAPIBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := apiBaseURL
	apiBaseURL = url
	t.Cleanup(func() { apiBaseURL = orig })
}

func TestLatestRelease(t *testing.T) {
	tarballBytes, digest := buildFixtureTarGz(t, []byte("fake-krayt-binary-contents"))
	const tarballName = "krayt_v0.6.1_darwin_arm64.tar.gz"
	srv := newFixtureServer(t, "v0.6.1", tarballName, tarballBytes, digest)
	defer srv.Close()
	withFixtureAPIBaseURL(t, srv.URL)

	rel, err := LatestRelease(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v0.6.1" {
		t.Errorf("TagName = %q, want v0.6.1", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("len(Assets) = %d, want 2", len(rel.Assets))
	}
}

func TestReleaseByTag(t *testing.T) {
	tarballBytes, digest := buildFixtureTarGz(t, []byte("fake-krayt-binary-contents"))
	const tarballName = "krayt_v0.6.1_darwin_arm64.tar.gz"
	srv := newFixtureServer(t, "v0.6.1", tarballName, tarballBytes, digest)
	defer srv.Close()
	withFixtureAPIBaseURL(t, srv.URL)

	rel, err := ReleaseByTag(context.Background(), srv.Client(), "v0.6.1")
	if err != nil {
		t.Fatalf("ReleaseByTag: %v", err)
	}
	if rel.TagName != "v0.6.1" {
		t.Errorf("TagName = %q, want v0.6.1", rel.TagName)
	}

	_, err = ReleaseByTag(context.Background(), srv.Client(), "v9.9.9")
	if err == nil {
		t.Fatal("ReleaseByTag(unknown tag): want error, got nil")
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("ReleaseByTag(unknown tag) error = %q, want it to mention the tag", err.Error())
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantErr      bool
		want         string
	}{
		{"darwin", "arm64", false, "krayt_v0.6.1_darwin_arm64.tar.gz"},
		{"darwin", "amd64", false, "krayt_v0.6.1_darwin_amd64.tar.gz"},
		{"linux", "amd64", false, "krayt_v0.6.1_linux_amd64.tar.gz"},
		{"linux", "arm64", true, ""},
		{"windows", "amd64", true, ""},
		{"plan9", "386", true, ""},
	}
	for _, c := range cases {
		got, err := AssetName(c.goos, c.goarch, "v0.6.1")
		if c.wantErr {
			if err == nil {
				t.Errorf("AssetName(%s,%s): want error, got %q", c.goos, c.goarch, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("AssetName(%s,%s): unexpected error: %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("AssetName(%s,%s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	const wantDigest = "28b4aacb00000000000000000000000000000000000000000000000000000000"
	realFixture := wantDigest + "  krayt_v0.6.1_darwin_amd64.tar.gz\n"
	m, err := ParseChecksums([]byte(realFixture))
	if err != nil {
		t.Fatalf("ParseChecksums(real fixture): %v", err)
	}
	if m["krayt_v0.6.1_darwin_amd64.tar.gz"] != wantDigest {
		t.Errorf("unexpected digest: %v", m)
	}

	multi := strings.Repeat("a", 64) + "  krayt_v0.6.1_darwin_arm64.tar.gz\n" +
		strings.Repeat("b", 64) + "  krayt_v0.6.1_darwin_amd64.tar.gz\n" +
		strings.Repeat("c", 64) + "  krayt_v0.6.1_linux_amd64.tar.gz\n"
	m, err = ParseChecksums([]byte(multi))
	if err != nil {
		t.Fatalf("ParseChecksums(multi): %v", err)
	}
	if len(m) != 3 {
		t.Errorf("len(m) = %d, want 3", len(m))
	}
	if _, ok := m["does-not-exist.tar.gz"]; ok {
		t.Error("missing filename unexpectedly present in map")
	}

	_, err = ParseChecksums([]byte("not-a-valid-line-at-all\n"))
	if err == nil {
		t.Fatal("ParseChecksums(malformed): want error, got nil")
	}
}

func TestDownloadAndVerify(t *testing.T) {
	content := []byte("fake-krayt-binary-contents")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/asset.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("success", func(t *testing.T) {
		destDir := t.TempDir()
		path, err := DownloadAndVerify(context.Background(), srv.Client(), srv.URL+"/asset.tar.gz", digest, destDir)
		if err != nil {
			t.Fatalf("DownloadAndVerify: %v", err)
		}
		if filepath.Dir(path) != destDir {
			t.Errorf("temp file %s not in destDir %s", path, destDir)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("content mismatch: got %q, want %q", got, content)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		destDir := t.TempDir()
		badDigest := "0" + digest[1:] // flip the leading hex char
		if badDigest == digest {
			badDigest = "1" + digest[1:]
		}
		_, err := DownloadAndVerify(context.Background(), srv.Client(), srv.URL+"/asset.tar.gz", badDigest, destDir)
		if err == nil {
			t.Fatal("DownloadAndVerify(bad digest): want error, got nil")
		}
		entries, err := os.ReadDir(destDir)
		if err != nil {
			t.Fatalf("ReadDir(destDir): %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("destDir not empty after mismatch: %v", entries)
		}
	})
}

func TestExtractBinary(t *testing.T) {
	content := []byte("fake-krayt-binary-contents")
	tarball, _ := buildFixtureTarGz(t, content)

	t.Run("round-trip", func(t *testing.T) {
		srcDir := t.TempDir()
		tarPath := filepath.Join(srcDir, "krayt.tar.gz")
		if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
			t.Fatalf("write fixture tarball: %v", err)
		}
		destDir := t.TempDir()
		binPath, err := ExtractBinary(tarPath, destDir)
		if err != nil {
			t.Fatalf("ExtractBinary: %v", err)
		}
		got, err := os.ReadFile(binPath)
		if err != nil {
			t.Fatalf("read extracted binary: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("content mismatch: got %q, want %q", got, content)
		}
		info, err := os.Stat(binPath)
		if err != nil {
			t.Fatalf("stat extracted binary: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", info.Mode().Perm())
		}
	})

	t.Run("zero entries", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.Close()
		_ = gz.Close()
		srcDir := t.TempDir()
		tarPath := filepath.Join(srcDir, "empty.tar.gz")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write empty tarball: %v", err)
		}
		if _, err := ExtractBinary(tarPath, t.TempDir()); err == nil {
			t.Fatal("ExtractBinary(zero entries): want error, got nil")
		}
	})

	t.Run("two entries", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, name := range []string{"krayt", "other"} {
			hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatalf("write header: %v", err)
			}
			if _, err := tw.Write(content); err != nil {
				t.Fatalf("write content: %v", err)
			}
		}
		_ = tw.Close()
		_ = gz.Close()
		srcDir := t.TempDir()
		tarPath := filepath.Join(srcDir, "two.tar.gz")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write two-entry tarball: %v", err)
		}
		if _, err := ExtractBinary(tarPath, t.TempDir()); err == nil {
			t.Fatal("ExtractBinary(two entries): want error, got nil")
		}
	})

	t.Run("wrong name", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		hdr := &tar.Header{Name: "not-krayt", Mode: 0o755, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write content: %v", err)
		}
		_ = tw.Close()
		_ = gz.Close()
		srcDir := t.TempDir()
		tarPath := filepath.Join(srcDir, "wrongname.tar.gz")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write wrong-name tarball: %v", err)
		}
		if _, err := ExtractBinary(tarPath, t.TempDir()); err == nil {
			t.Fatal("ExtractBinary(wrong name): want error, got nil")
		}
	})
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b    string
		want    int
		wantErr bool
	}{
		{"0.9.0", "0.10.0", -1, false},
		{"0.10.0", "0.9.0", 1, false},
		{"1.2.3", "1.2.3", 0, false},
		{"v1.2.3", "1.2.3", 0, false},
		{"v1.2.3", "v1.2.3", 0, false},
		{"1.2", "1.2.0", 0, true},
		{"1.2.x", "1.2.0", 0, true},
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if c.wantErr {
			if err == nil {
				t.Errorf("CompareVersions(%q,%q): want error, got %d", c.a, c.b, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("CompareVersions(%q,%q): unexpected error: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestApply(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		dir := t.TempDir()
		currentPath := filepath.Join(dir, "krayt")
		if err := os.WriteFile(currentPath, []byte("old-content"), 0o755); err != nil {
			t.Fatalf("write current: %v", err)
		}
		newPath := filepath.Join(dir, ".new-bin")
		if err := os.WriteFile(newPath, []byte("new-content"), 0o755); err != nil {
			t.Fatalf("write new: %v", err)
		}

		backupPath, err := Apply(currentPath, newPath)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if backupPath != currentPath+".bak" {
			t.Errorf("backupPath = %q, want %q", backupPath, currentPath+".bak")
		}
		got, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatalf("read current: %v", err)
		}
		if string(got) != "new-content" {
			t.Errorf("current content = %q, want new-content", got)
		}
		info, err := os.Stat(currentPath)
		if err != nil {
			t.Fatalf("stat current: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", info.Mode().Perm())
		}
		backup, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		if string(backup) != "old-content" {
			t.Errorf("backup content = %q, want old-content", backup)
		}
	})

	t.Run("non-writable dir", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permission bits don't block writes")
		}
		dir := t.TempDir()
		currentPath := filepath.Join(dir, "krayt")
		if err := os.WriteFile(currentPath, []byte("old-content"), 0o755); err != nil {
			t.Fatalf("write current: %v", err)
		}
		newPath := filepath.Join(t.TempDir(), "new-bin")
		if err := os.WriteFile(newPath, []byte("new-content"), 0o755); err != nil {
			t.Fatalf("write new: %v", err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod dir: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck

		_, err := Apply(currentPath, newPath)
		if err == nil {
			t.Fatal("Apply(non-writable dir): want error, got nil")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not mention dir %q", err.Error(), dir)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("restore dir perms: %v", err)
		}
		got, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatalf("read current: %v", err)
		}
		if string(got) != "old-content" {
			t.Errorf("current content changed: got %q, want old-content", got)
		}
	})
}
