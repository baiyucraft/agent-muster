package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const createNoWindow = 0x08000000 // Windows CREATE_NO_WINDOW.

var shimScript = regexp.MustCompile(`(?i)"%dp0%\\([^"\r\n]+\.(?:js|mjs|cjs))"`)

func probeCommand(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	if !isCommandShim(path) {
		return newProbeProcess(ctx, path, args...), nil
	}

	shimDir := filepath.Dir(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	match := shimScript.FindSubmatch(data)
	if len(match) != 2 {
		return nil, errors.New("无法从 Windows 命令 shim 解析 Agent 脚本")
	}

	script := filepath.Clean(filepath.Join(shimDir, filepath.FromSlash(strings.ReplaceAll(string(match[1]), `\`, `/`))))
	relative, err := filepath.Rel(shimDir, script)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("Agent 脚本必须位于命令 shim 目录内")
	}
	ext := strings.ToLower(filepath.Ext(script))
	if ext != ".js" && ext != ".mjs" && ext != ".cjs" {
		return nil, errors.New("Windows 命令 shim 未指向受支持的 JavaScript 文件")
	}
	if info, err := os.Stat(script); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Windows 命令 shim 指向的 Agent 脚本不存在")
	}

	node := filepath.Join(shimDir, "node.exe")
	if info, err := os.Stat(node); err != nil || !info.Mode().IsRegular() {
		node, err = exec.LookPath("node.exe")
		if err != nil {
			node, err = exec.LookPath("node")
		}
		if err != nil {
			return nil, errors.New("探测 Agent 需要 node.exe")
		}
	}
	return newProbeProcess(ctx, node, append([]string{script}, args...)...), nil
}

func newProbeProcess(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd
}

func isCommandShim(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}
