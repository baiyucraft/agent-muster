package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest(version string, url string) (Manifest, map[string][]byte) {
	contents := map[string][]byte{AppName: []byte("device executable bytes"), CLIName: []byte("helper executable bytes")}
	return manifestFor(version, url, contents)
}

func manifestFor(version, url string, contents map[string][]byte) (Manifest, map[string][]byte) {
	files := make(map[string]string, len(contents))
	for name, content := range contents {
		sum := sha256.Sum256(content)
		files[name] = hex.EncodeToString(sum[:])
	}
	archive := encodeArchive(contents)
	sum := sha256.Sum256(archive)
	return Manifest{
		Version: version, Platform: "windows/amd64", URL: url, Size: int64(len(archive)),
		SHA256: hex.EncodeToString(sum[:]), Files: files,
	}, contents
}

func encodeArchive(contents map[string][]byte) []byte {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range executableNames {
		entry, err := writer.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := entry.Write(contents[name]); err != nil {
			panic(err)
		}
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

// TestSignedOfferVerification 覆盖清单篡改、版本降级和平台不匹配。
func TestSignedOfferVerification(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := testManifest("1.2.3", "https://releases.example/muster.zip")
	offer, err := Sign(manifest, private)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		offer    Offer
		current  string
		platform string
		wantErr  bool
	}{
		{name: "valid", offer: offer, current: "1.2.2", platform: "windows/amd64"},
		{name: "downgrade", offer: offer, current: "1.2.3", platform: "windows/amd64", wantErr: true},
		{name: "wrong platform", offer: offer, current: "1.2.2", platform: "windows/arm64", wantErr: true},
		{name: "modified manifest", offer: Offer{Manifest: Manifest{Version: "1.2.4", Platform: offer.Manifest.Platform, URL: offer.Manifest.URL, Size: offer.Manifest.Size, SHA256: offer.Manifest.SHA256, Files: offer.Manifest.Files}, Signature: offer.Signature}, current: "1.2.2", platform: "windows/amd64", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Verify(test.offer, public, test.current, test.platform)
			if (err != nil) != test.wantErr {
				t.Fatalf("Verify() error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

// TestExtractChecksArchiveNamesAndFileDigests 确认解包拒绝摘要不符和越界路径。
func TestExtractChecksArchiveNamesAndFileDigests(t *testing.T) {
	temp := t.TempDir()
	contents := map[string][]byte{
		AppName: []byte("application"),
		CLIName: []byte("command helper"),
	}
	manifest, _ := manifestFor("1.0.1", "https://releases.example/muster.zip", contents)
	archivePath := filepath.Join(temp, "release.zip")
	if err := os.WriteFile(archivePath, encodeArchive(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archivePath, temp, manifest); err != nil {
		t.Fatal(err)
	}
	for name, expected := range contents {
		actual, err := os.ReadFile(filepath.Join(temp, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("extract %s = %q, %v", name, actual, err)
		}
	}

	contents[CLIName] = []byte("tampered")
	if err := os.Remove(filepath.Join(temp, CLIName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, encodeArchive(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archivePath, temp, manifest); err == nil {
		t.Fatal("tampered executable was accepted")
	}

	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	for _, name := range []string{CLIName, "../" + AppName} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(contents[AppName]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archivePath, t.TempDir(), manifest); err == nil {
		t.Fatal("archive entry escaping the staging directory was accepted")
	}
}

// TestFetchOfferLimitsAndStrictlyValidatesResponse 验证解析只接受限长的单份清单。
func TestFetchOfferLimitsAndStrictlyValidatesResponse(t *testing.T) {
	manifest, _ := testManifest("1.0.1", "https://releases.example/muster.zip")
	offer := Offer{Manifest: manifest, Signature: "signature"}
	valid, err := json.Marshal(offer)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		response string
		wantErr  bool
	}{
		{name: "valid", response: string(valid)},
		{name: "unknown field", response: `{"manifest":{},"signature":"x","extra":1}`, wantErr: true},
		{name: "trailing JSON", response: string(valid) + `{}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, test.response)
			}))
			defer server.Close()
			response, err := FetchOffer(context.Background(), server.URL+"/offer")
			if (err != nil) != test.wantErr {
				t.Fatalf("FetchOffer() error = %v, want error %v", err, test.wantErr)
			}
			if err == nil && (response.Signature != offer.Signature || response.Manifest.Version != offer.Manifest.Version) {
				t.Fatalf("FetchOffer() returned %+v", response)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), maxOfferSize+1))
	}))
	defer server.Close()
	_, err = FetchOffer(context.Background(), server.URL+"/large")
	if err == nil {
		t.Fatal("oversized offer was accepted")
	}
}

// TestDownloadArchiveChecksRedirectAndDigest 验证归档摘要和 HTTP 重定向边界。
func TestDownloadArchiveChecksRedirectAndDigest(t *testing.T) {
	contents := map[string][]byte{
		AppName: []byte("application"),
		CLIName: []byte("command helper"),
	}
	archive := encodeArchive(contents)
	manifest, _ := manifestFor("1.0.1", "https://releases.example/muster.zip", contents)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/release", http.StatusFound)
			return
		}
		w.Write(archive)
	}))
	defer server.Close()
	manifest.URL = server.URL + "/redirect"
	path := filepath.Join(t.TempDir(), "release.zip")
	if err := downloadArchive(context.Background(), manifest, path); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("download request count = %d, want 2", calls)
	}

	manifest.SHA256 = hex.EncodeToString(make([]byte, sha256.Size))
	if err := downloadArchive(context.Background(), manifest, filepath.Join(t.TempDir(), "bad.zip")); err == nil {
		t.Fatal("download with a mismatched digest was accepted")
	}

	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a different HTTP origin received the redirected request")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/offer", http.StatusFound)
	}))
	defer blocked.Close()
	manifest, _ = testManifest("1.0.1", blocked.URL+"/redirect")
	err := downloadArchive(context.Background(), manifest, filepath.Join(t.TempDir(), "blocked.zip"))
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || !strings.Contains(urlErr.Err.Error(), "invalid release download redirect") {
		t.Fatalf("off-host HTTP redirect error = %v", err)
	}
}
