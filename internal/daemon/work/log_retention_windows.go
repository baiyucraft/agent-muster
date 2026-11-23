package work

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/baiyucraft/agent-muster/internal/runreport"
	"golang.org/x/sys/windows"
)

func (worker *Worker) pruneRunLogs(ctx context.Context, cutoff time.Time) error {
	logs, err := openRunLogDirectory(filepath.Join(worker.root, "logs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("打开本机日志目录: %w", err)
	}
	defer logs.Close()

	var failedRuns int
	for ctx.Err() == nil {
		runIDs, readErr := logs.Readdirnames(64)
		for _, runID := range runIDs {
			if !logRunID.MatchString(runID) {
				continue
			}
			confirmed, err := worker.confirmedRunForLogRetention(ctx, runID)
			if err != nil {
				return err
			}
			if !confirmed {
				continue
			}
			worker.logMu.Lock()
			err = pruneRunLogDirectory(logs, runID, cutoff)
			worker.logMu.Unlock()
			if err != nil {
				failedRuns++
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errors.New("枚举本机日志目录失败")
		}
	}
	if failedRuns > 0 {
		return fmt.Errorf("%d 个运行的日志目录未能完整清理", failedRuns)
	}
	return ctx.Err()
}

func pruneRunLogDirectory(logs *os.File, runID string, cutoff time.Time) error {
	run, err := openLogChild(logs, runID, true, 0)
	if err != nil {
		return err
	}
	defer run.Close()
	names, err := run.Readdirnames(maxLogDirectories + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > maxLogDirectories {
		return errors.New("日志目录条目超过读取上限")
	}
	var pruneErr error
	for _, name := range names {
		if !runreport.IsLogInvocation(name) {
			continue
		}
		invocation, err := openLogChild(run, name, true, 0)
		if err != nil {
			pruneErr = errors.Join(pruneErr, err)
			continue
		}
		pruneErr = errors.Join(pruneErr,
			deleteExpiredLog(invocation, "stdout.log", cutoff),
			deleteExpiredLog(invocation, "stderr.log", cutoff),
			invocation.Close())
	}
	return pruneErr
}

// deleteExpiredLog 按打开的文件句柄检查时间和删除，避免检查路径后被替换。
func deleteExpiredLog(invocation *os.File, name string, cutoff time.Time) error {
	file, err := openLogChild(invocation, name, false, windows.DELETE)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err == nil && info.ModTime().Before(cutoff) {
		deleteOnClose := byte(1) // FILE_DISPOSITION_INFO.DeleteFile 为 Windows BOOLEAN。
		err = windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &deleteOnClose, 1)
	}
	return errors.Join(err, file.Close())
}
