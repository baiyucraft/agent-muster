// Package update 验证 Agent Muster 设备发布包，并负责 Windows 设备的暂存、更新和回滚。
package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

const (
	// AppName 是设备端安装目录中的桌面程序文件名。
	AppName = "agent-muster-device.exe"
	// CLIName 是与桌面程序一起发布和替换的协作命令文件名。
	CLIName = "muster.exe"
	// PublicKeyName 是未内嵌发布公钥时从安装目录读取的公钥文件名。
	PublicKeyName = "muster-update.pub"
	maxOfferSize  = 16 << 10
	maxRelease    = 128 << 20
)

var (
	// Version 是通过 ldflags 注入的安装版本。未装配版本的构建使用 0.0.0。
	Version = "0.0.0"
	// ReleasePublicKey 是可通过 ldflags 注入的 Base64 编码 Ed25519 发布公钥。
	ReleasePublicKey string
)

var executableNames = [...]string{CLIName, AppName}

// Manifest 描述目标版本、平台和归档。Size 是归档的字节数，Files 保存两个可执行文件的 SHA-256。
type Manifest struct {
	Version  string            `json:"version"`
	Platform string            `json:"platform"`
	URL      string            `json:"url"`
	Size     int64             `json:"size"`
	SHA256   string            `json:"sha256"`
	Files    map[string]string `json:"files"`
}

// Offer 是设备收到的发布清单和 Base64 编码的 Ed25519 签名。
type Offer struct {
	Manifest  Manifest `json:"manifest"`
	Signature string   `json:"signature"`
}

func (manifest Manifest) validate() error {
	if !deviceupdate.ValidVersion(manifest.Version) {
		return errors.New("invalid release version")
	}
	if manifest.Platform != "windows/amd64" && manifest.Platform != "windows/arm64" {
		return errors.New("invalid release platform")
	}
	if !deviceupdate.ValidURL(manifest.URL) {
		return errors.New("invalid release archive URL")
	}
	if manifest.Size < 1 || manifest.Size > maxRelease || !isDigest(manifest.SHA256) {
		return errors.New("invalid release archive size or SHA-256")
	}
	if len(manifest.Files) != len(executableNames) {
		return errors.New("invalid release file list")
	}
	for _, name := range executableNames {
		if !isDigest(manifest.Files[name]) {
			return fmt.Errorf("invalid SHA-256 for %s", name)
		}
	}
	return nil
}

func isDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && len(value) == 2*sha256.Size && strings.ToLower(value) == value
}

// Sign 校验发布清单，再签署结构化清单编码后的 JSON。
func Sign(manifest Manifest, key ed25519.PrivateKey) (Offer, error) {
	if err := manifest.validate(); err != nil {
		return Offer{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return Offer{}, errors.New("invalid release private key")
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return Offer{}, err
	}
	return Offer{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))}, nil
}

// Verify 使用本地信任的发布公钥核验签名，并要求发行平台匹配、目标版本高于已安装版本。
func Verify(offer Offer, key ed25519.PublicKey, currentVersion, platform string) error {
	if err := offer.Manifest.validate(); err != nil {
		return err
	}
	if platform != offer.Manifest.Platform {
		return errors.New("release targets another platform")
	}
	if !deviceupdate.ValidVersion(currentVersion) || deviceupdate.CompareVersion(offer.Manifest.Version, currentVersion) <= 0 {
		return errors.New("release version must be newer than the installed version")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(offer.Signature)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("invalid release signature or public key")
	}
	payload, err := json.Marshal(offer.Manifest)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, payload, signature) {
		return errors.New("release signature does not match the installed public key")
	}
	return nil
}

func platform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// LoadPublicKey 优先读取内嵌公钥，否则读取随安装部署的公钥文件。
func LoadPublicKey() (ed25519.PublicKey, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return loadPublicKey(filepath.Dir(exe))
}

func loadPublicKey(installDir string) (ed25519.PublicKey, error) {
	raw := ReleasePublicKey
	if raw == "" {
		file, err := os.Open(filepath.Join(installDir, PublicKeyName))
		if err != nil {
			return nil, fmt.Errorf("load trusted release key: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 256))
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(string(data))
	}
	key, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid trusted release key")
	}
	return ed25519.PublicKey(key), nil
}

// FetchOffer 按大小与超时限制下载并解析清单。调用方必须在暂存前验证签名。
func FetchOffer(ctx context.Context, url string) (Offer, error) {
	response, err := download(ctx, url)
	if err != nil {
		return Offer{}, err
	}
	defer response.Body.Close()
	var offer Offer
	if err := decodeJSON(response.Body, maxOfferSize, &offer); err != nil {
		return Offer{}, err
	}
	return offer, nil
}

func download(ctx context.Context, url string) (*http.Response, error) {
	if !deviceupdate.ValidURL(url) {
		return nil, errors.New("invalid release download URL")
	}
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 发布资产可能走 CDN。使用 HTTP 时仅允许同源 HTTP 重定向。
			from := via[len(via)-1].URL
			if len(via) > 4 || !deviceupdate.ValidURL(req.URL.String()) ||
				(req.URL.Scheme == "http" && (from.Scheme != "http" || !strings.EqualFold(from.Host, req.URL.Host))) {
				return errors.New("invalid release download redirect")
			}
			req.Header.Del("Referer")
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("release download failed: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("release download returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func decodeJSON(input io.Reader, limit int64, result any) error {
	raw, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return errors.New("release record exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing release data")
	}
	return nil
}

func downloadArchive(ctx context.Context, manifest Manifest, path string) error {
	response, err := download(ctx, manifest.URL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength >= 0 && response.ContentLength != manifest.Size {
		return errors.New("release download size does not match its manifest")
	}
	return writeVerifiedFile(path, response.Body, manifest.Size, manifest.SHA256)
}

func extract(archivePath, dir string, manifest Manifest) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) != len(executableNames) {
		return errors.New("release archive must contain the two signed executables")
	}
	// 固定文件名约束解包路径，剩余字节数约束归档的总解压体积。
	var remaining uint64 = maxRelease
	for _, file := range archive.File {
		digest, exists := manifest.Files[file.Name]
		if !exists || !file.Mode().IsRegular() || file.UncompressedSize64 == 0 || file.UncompressedSize64 > remaining {
			return errors.New("invalid release archive entry")
		}
		remaining -= file.UncompressedSize64
		reader, err := file.Open()
		if err != nil {
			return err
		}
		err = writeVerifiedFile(filepath.Join(dir, file.Name), reader, int64(file.UncompressedSize64), digest)
		err = errors.Join(err, reader.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func writeVerifiedFile(path string, input io.Reader, size int64, digest string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(input, size+1))
	if err == nil && (count != size || hex.EncodeToString(hash.Sum(nil)) != digest) {
		err = errors.New("release file checksum or size does not match its manifest")
	}
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxRelease+1))
	if err != nil {
		return "", err
	}
	if size > maxRelease {
		return "", errors.New("release executable exceeds its size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
