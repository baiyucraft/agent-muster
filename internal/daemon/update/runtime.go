package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

const (
	updateDir  = ".muster-update"
	helperName = "apply.exe"
	helperFlag = "--apply-device-update"
	readyName  = "helper-ready"
	healthName = "startup-healthy"
	recordName = "offer.json"
	activeName = "active.json"
	backupDir  = "previous"
)

// BackgroundLaunchEnv 让更新器后台重启桌面应用，旧版本可忽略此环境变量。
const BackgroundLaunchEnv = "MUSTER_DEVICE_BACKGROUND"

var (
	profilePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	assignmentPattern = regexp.MustCompile(`^upd-[a-f0-9]{32}$`)
	stagePattern      = regexp.MustCompile(`^release-[0-9]+$`)
)

// Prepared 持有本机暂存的已验签发布包。调用方应完成 Start，或调用 Discard 释放暂存文件。
type Prepared struct {
	install string
	stage   string
	offer   Offer
}

// Result 是更新器按设备配置落盘的终态，可在桌面程序重启后用 AssignmentID 补报。
type Result struct {
	AssignmentID string `json:"assignment_id"`
	Version      string `json:"version"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
}

type applicationRecord struct {
	Offer        Offer  `json:"offer"`
	AssignmentID string `json:"assignment_id"`
	Profile      string `json:"profile"`
}

func installDirectory() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(filepath.Base(exe), AppName) {
		return "", fmt.Errorf("device updates require an installation of %s", AppName)
	}
	return filepath.Dir(exe), nil
}

// Discard 清理调用方仍持有的暂存目录。Start 成功后由更新器负责清理。
func (prepared *Prepared) Discard() {
	if prepared.stage != "" {
		_ = os.RemoveAll(prepared.stage)
	}
}

// LastResult 读取指定设备配置的最新终态。尚无终态时返回 os.ErrNotExist 对应的文件错误。
func LastResult(profile string) (Result, error) {
	if !profilePattern.MatchString(profile) {
		return Result{}, errors.New("invalid device profile")
	}
	install, err := installDirectory()
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = readRecord(resultPath(install, profile), &result)
	return result, err
}

func resultPath(install, profile string) string {
	return filepath.Join(install, updateDir, "result-"+profile+".json")
}

func readRecord(path string, result any) error {
	file, err := openRecord(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return decodeJSON(file, 2*maxOfferSize, result)
}

func validatedRecord(stage string) (applicationRecord, error) {
	var record applicationRecord
	if err := readRecord(filepath.Join(stage, recordName), &record); err != nil {
		return applicationRecord{}, err
	}
	if !profilePattern.MatchString(record.Profile) || !assignmentPattern.MatchString(record.AssignmentID) ||
		!deviceupdate.ValidVersion(record.Offer.Manifest.Version) {
		return applicationRecord{}, errors.New("invalid staged release record")
	}
	return record, nil
}

func atomicWriteRecord(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return replaceWithReader(path, bytes.NewReader(data), 2*maxOfferSize)
}

func replaceWithFile(destination, source string) error {
	if err := requireRegularFile(source); err != nil {
		return err
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	return replaceWithReader(destination, file, maxRelease)
}

func replaceWithReader(destination string, input io.Reader, limit int64) error {
	// 先完整写入并同步同卷临时文件，再替换目标，便于其他进程读取完整记录。
	file, err := os.CreateTemp(filepath.Dir(destination), ".muster-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	count, err := io.Copy(file, io.LimitReader(input, limit+1))
	if err == nil && (count == 0 || count > limit) {
		err = errors.New("invalid replacement file size")
	}
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return replaceFile(file.Name(), destination)
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", filepath.Base(path))
	}
	return nil
}
