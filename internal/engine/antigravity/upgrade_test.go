package antigravity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildTestTarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(data)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckAndUpgradeCLIFallbackDownload(t *testing.T) {
	binDir := t.TempDir()
	binName := "agy"
	binPath := filepath.Join(binDir, binName)
	oldELF := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("old-agy-binary")...)
	if err := os.WriteFile(binPath, oldELF, 0o755); err != nil {
		t.Fatal(err)
	}

	newELF := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("new-agy-1.6.0-binary")...)
	archiveBytes := buildTestTarGz(t, "antigravity", newELF)
	sum := sha512.Sum512(archiveBytes)
	sumHex := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/manifests/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliManifest{
			Version: "1.6.0",
			URL:     srv.URL + "/dl/cli_linux_x64.tar.gz",
			SHA512:  sumHex,
		})
	})
	mux.HandleFunc("/dl/cli_linux_x64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	replaced := false
	c := &Client{
		Bin:               binPath,
		HTTP:              srv.Client(),
		ManifestBaseURL:   srv.URL,
		AllowInsecureHTTP: true,
		SkipSmokeExec:     true,
		SkipBuiltinUpdate: true,
		GOOS:              "linux",
		GOARCH:            "amd64",
		RunUpdateCmd: func(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
			if len(args) == 1 && args[0] == "--version" {
				if replaced {
					return "agy 1.6.0", nil
				}
				return "agy 1.5.18", nil
			}
			return "", nil
		},
	}

	st := c.CheckCLIUpgrade(context.Background())
	if st.Error != "" || !st.UpdateAvailable || st.Current != "1.5.18" || st.Latest != "1.6.0" || !st.Writable {
		t.Fatalf("unexpected CLIStatus: %#v", st)
	}

	res, err := c.UpgradeCLI(context.Background())
	if err != nil {
		t.Fatalf("UpgradeCLI: %v", err)
	}
	replaced = true
	if !res.Changed || res.OldVersion != "1.5.18" || res.NewVersion != "1.6.0" {
		t.Fatalf("unexpected CLIUpgradeResult: %#v", res)
	}
	gotBytes, err := os.ReadFile(binPath)
	if err != nil || !bytes.Equal(gotBytes, newELF) {
		t.Fatalf("binary not replaced: err=%v len=%d", err, len(gotBytes))
	}
	prevBytes, err := os.ReadFile(binPath + ".prev")
	if err != nil || !bytes.Equal(prevBytes, oldELF) {
		t.Fatalf("missing .prev backup: err=%v", err)
	}

	// Untrusted host must be rejected when AllowInsecureHTTP=false.
	cStrict := &Client{ManifestBaseURL: "https://evil.example.com"}
	if err := cStrict.validateDownloadURL("https://evil.example.com/a.tar.gz"); err == nil || !strings.Contains(err.Error(), "白名单") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}
