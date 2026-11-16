package upgrade

import (
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
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.9.1", "v0.9.2", -1, true},
		{"0.9.2", "v0.9.2", 0, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0-rc.1", "v1.0.0", -1, true},
		{"v1.0.0", "v1.0.0-beta.1", 1, true},
		{"dev", "v0.9.2", 0, false},
		{"v0.9.1", "invalid", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.a, tc.b)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = (%d, %v), want (%d, %v)", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAssetNameForAndParseSHA256Sums(t *testing.T) {
	if got, err := AssetNameFor("linux", "amd64"); err != nil || got != "agent-bot-linux-amd64" {
		t.Fatalf("linux/amd64: %q %v", got, err)
	}
	if got, err := AssetNameFor("windows", "amd64"); err != nil || got != "agent-bot-windows-amd64.exe" {
		t.Fatalf("windows/amd64: %q %v", got, err)
	}
	if got, err := AssetNameFor("darwin", "arm64"); err != nil || got != "agent-bot-darwin-arm64" {
		t.Fatalf("darwin/arm64: %q %v", got, err)
	}
	if _, err := AssetNameFor("freebsd", "386"); err == nil {
		t.Fatal("expected error for unsupported OS/arch")
	}

	digest := strings.Repeat("ab", 32)
	content := "# checksums\n" +
		strings.Repeat("11", 32) + "  agent-bot-darwin-arm64\n" +
		strings.ToUpper(digest) + " *dist/agent-bot-linux-amd64\n"
	got, err := ParseSHA256Sums(content, "agent-bot-linux-amd64")
	if err != nil || got != digest {
		t.Fatalf("got %q err=%v, want %q", got, err, digest)
	}
	if _, err := ParseSHA256Sums(content, "agent-bot-windows-amd64.exe"); err == nil {
		t.Fatal("expected missing asset error")
	}
}

func TestStageCommitAndRollbackAcrossOS(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			assetName, err := AssetNameFor(goos, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if goos == "windows" {
				payload = []byte("MZ-fake-pe-agent-bot-v1.1.0")
			} else {
				payload = append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-fake-elf-agent-bot-v1.1.0")...)
			}
			sum := sha256.Sum256(payload)
			sumHex := hex.EncodeToString(sum[:])
			sumsBody := fmt.Sprintf("%s  %s\n", sumHex, assetName)

			var srv *httptest.Server
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/rshun/agent-bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name":   "v1.1.0",
					"html_url":   srv.URL + "/releases/tag/v1.1.0",
					"draft":      false,
					"prerelease": false,
					"assets": []map[string]string{
						{"name": assetName, "browser_download_url": srv.URL + "/dl/" + assetName},
						{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
					},
				})
			})
			mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(payload)
			})
			mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(sumsBody))
			})
			srv = httptest.NewServer(mux)
			defer srv.Close()

			dir := t.TempDir()
			binName := "agent-bot"
			if goos == "windows" {
				binName = "agent-bot.exe"
			}
			targetPath := filepath.Join(dir, binName)
			oldBytes := []byte("old-binary-v1.0.0")
			if err := os.WriteFile(targetPath, oldBytes, 0o755); err != nil {
				t.Fatal(err)
			}

			smokeCalled := false
			upg := &Upgrader{
				CurrentVersion:    "v1.0.0",
				Paths:             ExePaths{LaunchPath: targetPath, TargetPath: targetPath},
				GOOS:              goos,
				GOARCH:            "amd64",
				APIBase:           srv.URL,
				HTTP:              srv.Client(),
				AllowInsecureHTTP: true,
				RunCmd: func(_ context.Context, _ time.Duration, _ string, args ...string) (string, error) {
					if len(args) == 1 && args[0] == "--version" {
						smokeCalled = true
						return "agent-bot v1.1.0\n", nil
					}
					return "", fmt.Errorf("unexpected args: %v", args)
				},
			}

			st := upg.CheckBot(context.Background())
			if !st.Writable || !st.UpdateAvailable || st.Latest != "v1.1.0" {
				t.Fatalf("CheckBot unexpected: %+v", st)
			}

			// Version lock mismatch must reject cleanly.
			if _, err := upg.StageBotUpgrade(context.Background(), "v1.0.9"); err == nil || !strings.Contains(err.Error(), "v1.1.0") {
				t.Fatalf("expected tag lock mismatch error, got %v", err)
			}

			staged, err := upg.StageBotUpgrade(context.Background(), "v1.1.0")
			if err != nil {
				t.Fatalf("StageBotUpgrade: %v", err)
			}
			if !staged.Changed || !smokeCalled || staged.NewVersion != "v1.1.0" {
				t.Fatalf("unexpected staged result: %+v smokeCalled=%v", staged, smokeCalled)
			}

			prevPath, err := commitBinaryForOS(goos, targetPath, staged.StagedPath)
			if err != nil {
				t.Fatalf("commitBinaryForOS: %v", err)
			}
			if prevPath != targetPath+".prev" {
				t.Fatalf("prevPath = %q, want %q", prevPath, targetPath+".prev")
			}

			gotNew, err := os.ReadFile(targetPath)
			if err != nil || string(gotNew) != string(payload) {
				t.Fatalf("target binary mismatch: %q err=%v", string(gotNew), err)
			}

			if err := rollbackBinaryForOS(goos, targetPath); err != nil {
				t.Fatalf("rollbackBinaryForOS: %v", err)
			}
			gotRolled, err := os.ReadFile(targetPath)
			if err != nil || string(gotRolled) != string(oldBytes) {
				t.Fatalf("rollback binary mismatch: %q err=%v", string(gotRolled), err)
			}
		})
	}
}

func TestStageRejectsTamperedHashBadMagicAndUntrustedHost(t *testing.T) {
	badMagic := []byte("HTML-error-page-not-an-elf")
	elfPayload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-tampered-binary")...)
	wrongHex := strings.Repeat("00", 32)
	serveBadMagic := false

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/rshun/agent-bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.1.0",
			"assets": []map[string]string{
				{"name": "agent-bot-linux-amd64", "browser_download_url": srv.URL + "/dl/agent-bot-linux-amd64"},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
			},
		})
	})
	mux.HandleFunc("/dl/agent-bot-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		if serveBadMagic {
			_, _ = w.Write(badMagic)
			return
		}
		_, _ = w.Write(elfPayload)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		if serveBadMagic {
			sum := sha256.Sum256(badMagic)
			_, _ = fmt.Fprintf(w, "%s  agent-bot-linux-amd64\n", hex.EncodeToString(sum[:]))
			return
		}
		_, _ = fmt.Fprintf(w, "%s  agent-bot-linux-amd64\n", wrongHex)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "agent-bot")
	if err := os.WriteFile(targetPath, []byte("original-safe-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	upg := &Upgrader{
		CurrentVersion:    "v1.0.0",
		Paths:             ExePaths{LaunchPath: targetPath, TargetPath: targetPath},
		GOOS:              "linux",
		GOARCH:            "amd64",
		APIBase:           srv.URL,
		HTTP:              srv.Client(),
		AllowInsecureHTTP: true,
		SkipSmokeExec:     true,
	}
	if _, err := upg.StageBotUpgrade(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected SHA-256 mismatch error, got %v", err)
	}

	serveBadMagic = true
	if _, err := upg.StageBotUpgrade(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "可执行文件格式") {
		t.Fatalf("expected executable header error, got %v", err)
	}

	stillOrig, _ := os.ReadFile(targetPath)
	if string(stillOrig) != "original-safe-binary" {
		t.Fatalf("original binary was modified despite failed verification: %q", string(stillOrig))
	}

	// Production mode (AllowInsecureHTTP=false) rejects plain HTTP and untrusted domains.
	prodUpg := &Upgrader{
		CurrentVersion: "v1.0.0",
		Paths:          ExePaths{LaunchPath: targetPath, TargetPath: targetPath},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}
	if err := prodUpg.validateDownloadURL("http://github.com/rshun/agent-bot/releases/download/v1.1.0/agent-bot-linux-amd64"); err == nil {
		t.Fatal("accepted non-HTTPS URL in production mode")
	}
	if err := prodUpg.validateDownloadURL("https://evil.example.com/agent-bot-linux-amd64"); err == nil {
		t.Fatal("accepted untrusted domain in production mode")
	}
}

func TestDevBuildRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent-bot")
	_ = os.WriteFile(target, []byte("x"), 0o755)

	upg := &Upgrader{
		CurrentVersion: "dev",
		Paths:          ExePaths{LaunchPath: target, TargetPath: target},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}
	if _, err := upg.StageBotUpgrade(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("expected dev build rejection, got %v", err)
	}
}
