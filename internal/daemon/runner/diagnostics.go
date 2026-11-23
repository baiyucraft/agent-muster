package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

type captureKey struct{}

type runCapture struct {
	directory           string
	invocationDirectory string
	invocations         int
	report              runreport.Diagnostics
}

// WithDiagnostics 让本轮 CLI 将原始输出保存在设备目录，并收集可上报的诊断。
// 调用方须在本轮全部调用退出后再读取结果。
func WithDiagnostics(ctx context.Context, directory string) (context.Context, func() *runreport.Diagnostics) {
	return withDiagnostics(ctx, &runCapture{directory: directory})
}

// WithReviewDiagnostics 为一次独立 Review 保留日志目录并替换继承的修复诊断。
// 目录预留失败时返回空引用，CLI 仍可收集退出码和用量。调用结束后再读取诊断。
func WithReviewDiagnostics(ctx context.Context, directory string) (context.Context, string, func() *runreport.Diagnostics) {
	capture := &runCapture{}
	var invocation string
	if directory != "" && os.MkdirAll(directory, 0700) == nil {
		if path, err := allocateLogDirectory(directory, "review-"); err == nil {
			capture.invocationDirectory = path
			invocation = filepath.Base(path)
		}
	}
	ctx, result := withDiagnostics(ctx, capture)
	return ctx, invocation, result
}

func withDiagnostics(ctx context.Context, capture *runCapture) (context.Context, func() *runreport.Diagnostics) {
	return context.WithValue(ctx, captureKey{}, capture), func() *runreport.Diagnostics {
		if capture.invocations == 0 && capture.report.ErrorKind == "" {
			return nil
		}
		return &capture.report
	}
}

func diagnostics(ctx context.Context) *runCapture {
	capture, _ := ctx.Value(captureKey{}).(*runCapture)
	return capture
}

func recordUsage(ctx context.Context, usage *runreport.Usage) {
	if capture := diagnostics(ctx); capture != nil && capture.invocations == 1 {
		capture.report.Usage = usage
	}
}

type streamLog struct {
	file  *os.File
	bytes int64
	err   bool
}

// Write 保留与 CLI 解析独立的输出副本。写满后继续排空管道。
func (log *streamLog) Write(p []byte) (int, error) {
	if log.file != nil && !log.err {
		n, err := log.file.Write(p)
		log.bytes += int64(n)
		log.err = err != nil || n != len(p)
	}
	return len(p), nil
}

func (log *streamLog) finish() (*int64, bool) {
	if log.file == nil {
		return nil, true
	}
	syncErr := log.file.Sync()
	closeErr := log.file.Close()
	incomplete := log.err || syncErr != nil || closeErr != nil
	if incomplete && log.bytes == 0 {
		return nil, true
	}
	return &log.bytes, incomplete
}

func (capture *runCapture) openStreams() (*streamLog, *streamLog) {
	capture.invocations++
	capture.report.Usage = nil
	capture.report.ExitCode = nil
	stdout, stderr := &streamLog{}, &streamLog{}
	path := capture.invocationDirectory
	if path == "" && capture.directory != "" {
		if err := os.MkdirAll(capture.directory, 0700); err != nil {
			return stdout, stderr
		}
		// 补写或恢复调用使用新目录，避免覆盖先前日志。
		path, _ = allocateLogDirectory(capture.directory, "cli-")
	}
	if path != "" {
		stdout.file, _ = os.OpenFile(filepath.Join(path, "stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		stderr.file, _ = os.OpenFile(filepath.Join(path, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	return stdout, stderr
}

// allocateLogDirectory 使用满足上报契约的十进制序号，并以 Mkdir 独占名称。
// 每个运行目录单独编号，进程重启和并发执行不会覆盖此前日志。
func allocateLogDirectory(parent, prefix string) (string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", err
	}
	var next uint64 = 1
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		digits := strings.TrimPrefix(name, prefix)
		if len(digits) == 0 || len(digits) > 10 || !decimalDigits(digits) {
			continue
		}
		number, err := strconv.ParseUint(digits, 10, 34)
		if err == nil && number >= next {
			next = number + 1
		}
	}
	const maxSequence = 9999999999
	for next <= maxSequence {
		path := filepath.Join(parent, prefix+strconv.FormatUint(next, 10))
		err := os.Mkdir(path, 0700)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		next++
	}
	return "", errors.New("CLI 日志调用序号已耗尽")
}

func decimalDigits(value string) bool {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func (capture *runCapture) finishStreams(stdout, stderr *streamLog, incomplete bool) {
	out, outErr := stdout.finish()
	errOut, errErr := stderr.finish()
	addCapturedBytes(&capture.report.StdoutBytes, out)
	addCapturedBytes(&capture.report.StderrBytes, errOut)
	capture.report.CaptureIncomplete = capture.report.CaptureIncomplete || incomplete || outErr || errErr
}

func addCapturedBytes(total **int64, value *int64) {
	if value == nil {
		return
	}
	if *total == nil {
		*total = value
		return
	}
	**total += *value
}

func capturedReader(reader io.Reader, log *streamLog) io.Reader {
	if log == nil {
		return reader
	}
	return io.TeeReader(reader, log)
}
