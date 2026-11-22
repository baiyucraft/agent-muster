// Package main 提供设备发布密钥的生成命令，以及归档和清单的签名命令。
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/daemon/update"
	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: muster-release <keygen|sign> [flags]")
	}
	switch args[0] {
	case "keygen":
		return generate(args[1:], stdout)
	case "sign":
		return sign(args[1:], stdout)
	default:
		return fmt.Errorf("unknown release operation %q", args[0])
	}
}

func generate(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	privatePath := flags.String("private", "", "offline Ed25519 private key output")
	publicPath := flags.String("public", "", "trusted Ed25519 public key output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *privatePath == "" || *publicPath == "" {
		return errors.New("both --private and --public are required")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	private := base64.StdEncoding.EncodeToString(privateKey)
	if err := writeExclusive(*privatePath, []byte(private+"\n"), 0600); err != nil {
		return err
	}
	public := base64.StdEncoding.EncodeToString(publicKey)
	if err := writeExclusive(*publicPath, []byte(public+"\n"), 0600); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "created an Ed25519 signing key pair; keep the private key offline")
	return err
}

func sign(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	version := flags.String("version", "", "strict SemVer release version")
	platform := flags.String("platform", "", "target platform")
	archiveURL := flags.String("url", "", "HTTP(S) release archive URL")
	directory := flags.String("dir", "", "directory containing the two release executables")
	privatePath := flags.String("private-key", "", "base64 Ed25519 private key file")
	publicKeyPath := flags.String("public-key", "", "base64 trusted public key file")
	keyOutputPath := flags.String("key-output", "", "trusted public key copied beside the signed offer")
	outputPath := flags.String("output", "", "signed release offer JSON output")
	archivePath := flags.String("archive", "", "release ZIP output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !deviceupdate.ValidVersion(*version) || *directory == "" || *privatePath == "" || *publicKeyPath == "" ||
		*keyOutputPath == "" ||
		*outputPath == "" || *archivePath == "" || *archiveURL == "" {
		return errors.New("provide --version --platform --url --dir --private-key --public-key --key-output --archive --output")
	}
	if *platform != "windows/amd64" && *platform != "windows/arm64" {
		return errors.New("supported targets are windows/amd64 and windows/arm64")
	}
	if !deviceupdate.ValidURL(*archiveURL) {
		return errors.New("invalid release archive URL")
	}
	privateBytes, err := os.ReadFile(*privatePath)
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(privateBytes)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return errors.New("private key must be base64-encoded Ed25519")
	}
	private := ed25519.PrivateKey(decoded)
	publicKeyBytes, err := os.ReadFile(*publicKeyPath)
	if err != nil {
		return err
	}
	publicKey, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(publicKeyBytes)))
	// 公钥副本用于初次安装，必须匹配签署本次发行包的私钥。
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.NewKeyFromSeed(private.Seed()).Equal(private) ||
		!private.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(publicKey)) {
		return errors.New("trusted public key does not match the release signing key")
	}
	files, err := createArchive(*archivePath, *directory)
	if err != nil {
		return err
	}
	size, err := fileSize(*archivePath)
	if err != nil {
		return err
	}
	archiveHash, err := hashFile(*archivePath)
	if err != nil {
		return err
	}
	offer, err := update.Sign(update.Manifest{
		Version: *version, Platform: *platform, URL: *archiveURL, Size: size,
		SHA256: archiveHash, Files: files,
	}, private)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(offer, "", "  ")
	if err != nil {
		return err
	}
	if err := writeExclusive(*outputPath, append(encoded, '\n'), 0644); err != nil {
		return err
	}
	if err := writeExclusive(*keyOutputPath, append([]byte(strings.TrimSpace(string(publicKeyBytes))), '\n'), 0644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "signed %s (%s, %d bytes)\n", *version, *platform, size)
	return err
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	return file.Close()
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%s must be a non-empty regular file", filepath.Base(path))
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return 0, errors.New("release archive is empty")
	}
	return info.Size(), nil
}

func createArchive(path, directory string) (map[string]string, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	archive := zip.NewWriter(file)
	files := make(map[string]string, 2)
	for _, name := range []string{update.CLIName, update.AppName} {
		source := filepath.Join(directory, name)
		digest, err := addFile(archive, source, name)
		if err != nil {
			archive.Close()
			file.Close()
			os.Remove(path)
			return nil, err
		}
		files[name] = digest
	}
	if err := errors.Join(archive.Close(), file.Close()); err != nil {
		os.Remove(path)
		return nil, err
	}
	return files, nil
}

func addFile(archive *zip.Writer, path, name string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%s must be a non-empty regular file", name)
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return "", err
	}
	header.Name = name
	header.Method = zip.Deflate
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return "", err
	}
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	// 归档写入和摘要计算读取同一份字节，使清单准确绑定包内内容。
	_, copyErr := io.Copy(io.MultiWriter(writer, hash), source)
	return hex.EncodeToString(hash.Sum(nil)), errors.Join(copyErr, source.Close())
}
