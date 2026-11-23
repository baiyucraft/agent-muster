package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var dshUnsupportedLogPattern = regexp.MustCompile(`^ uses log format v[0-9]+, (?:but this harness reads only v[0-9]+: the log was written by a newer harness — upgrade the harness to open it|older than the supported v[0-9]+, and this build ships no upgrade path for it)(?: \(raw log: .+\))?$`)

// ErrProcessExitUnconfirmed 表示 Windows 未确认本轮所属进程树已全部退出。
var ErrProcessExitUnconfirmed = errors.New("无法确认本轮 Agent 进程树已退出")

// ErrSessionBusy 表示 dsh 启动续接时发现该会话已有写入者，且本轮进程树已退出。
var ErrSessionBusy = errors.New("dsh 原生会话正在被占用")

// ErrSessionUnavailable 表示 dsh 无法打开指定的持久化会话，且本轮进程树已退出。
var ErrSessionUnavailable = errors.New("dsh 原生会话不可用")

// RunDSH 从标准输入向无界面的 dsh 传递提示词，并在解析到原生会话事件后回调 session。
func RunDSH(ctx context.Context, directory, prompt, sessionID string, started func() error, session func(string) error) (string, error) {
	return RunDSHWithEnvironment(ctx, directory, prompt, sessionID, nil, started, session)
}

// RunDSHWithEnvironment 在项目目录运行 dsh，并注入许可的本机协作变量。
// 协作轮次可加载设备管理员设置的 headless profile 补丁，续接沿用同一配置入口。
func RunDSHWithEnvironment(ctx context.Context, directory, prompt, sessionID string, env []string, started func() error, session func(string) error) (string, error) {
	path, err := exec.LookPath("dsh")
	if err != nil {
		return "", errors.New("无法启动 dsh，请检查本机安装和 PATH")
	}
	args := []string{"--profile", "headless"}
	if patch := os.Getenv("MUSTER_DSH_MCP_PATCH"); patch != "" && len(env) != 0 {
		if !filepath.IsAbs(patch) {
			return "", errors.New("MUSTER_DSH_MCP_PATCH 必须是绝对文件路径")
		}
		if info, err := os.Stat(patch); err != nil || !info.Mode().IsRegular() {
			return "", errors.New("MUSTER_DSH_MCP_PATCH 指定的文件不可用")
		}
		args = append(args, "--patch", patch)
	}
	args = append(args, "--json")
	if sessionID != "" {
		if !sessionIDPattern.MatchString(sessionID) {
			return "", errors.New("无效的 dsh 会话 ID")
		}
		args = append(args, "--session-id", sessionID)
	}
	cmd, err := probeCommand(ctx, path, append(args, "-")...)
	if err != nil {
		return "", err
	}
	cmd.Dir = directory
	cmd.Env = filteredAgentEnvironment(cmd.Environ(), env)
	var response dshResponse
	exitCode, err := runInJob(ctx, cmd, prompt, started, func(stdout io.Reader) error {
		return response.read(stdout, sessionID, session)
	})
	if err != nil {
		return response.final, err
	}
	if err := response.exitError(sessionID, exitCode); err != nil {
		return response.final, err
	}
	if strings.TrimSpace(response.final) == "" {
		return "", errors.New("dsh 未提供最终回复")
	}
	recordUsage(ctx, response.usage.result())
	return response.final, nil
}

type dshResponse struct {
	sessionID string
	final     string
	usage     dshUsage
	errorText string
	lines     int
}

func (response *dshResponse) read(stdout io.Reader, expectedSession string, session func(string) error) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 8192), 4<<20)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		response.lines++
		var event struct {
			Type      string          `json:"type"`
			SessionID string          `json:"sessionId"`
			Text      string          `json:"text"`
			Message   string          `json:"message"`
			Phase     string          `json:"phase"`
			Turn      *int            `json:"turn"`
			Step      *int            `json:"step"`
			Usage     json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			// CLI 可能混入启动诊断。带类型的终态事件才参与结果解析。
			if event.Type == "status" && event.Phase == "step_end" {
				response.usage.invalid = true
			}
			continue
		}
		switch event.Type {
		case "session":
			if !sessionIDPattern.MatchString(event.SessionID) || expectedSession != "" && event.SessionID != expectedSession {
				return errors.New("dsh 返回了不匹配的会话 ID")
			}
			if response.sessionID != "" {
				if response.sessionID != event.SessionID {
					return errors.New("dsh 在同一轮发出了多个会话 ID")
				}
				continue
			}
			if err := session(event.SessionID); err != nil {
				return fmt.Errorf("无法保存 dsh 会话: %w", err)
			}
			response.sessionID = event.SessionID
		case "status":
			if event.Phase != "step_end" {
				continue
			}
			if response.sessionID == "" || event.Turn == nil || event.Step == nil {
				response.usage.invalid = true
				continue
			}
			response.usage.add(*event.Turn, *event.Step, event.Usage)
		case "final":
			response.final = event.Text
		case "error":
			response.errorText = event.Message
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("无法读取 dsh 输出: %w", err)
	}
	// 缺少会话事件时先等待正常退出，避免在读取退出码之前取消进程。
	return nil
}

func (response *dshResponse) exitError(expectedSession string, exitCode uint32) error {
	if exitCode == 1 && expectedSession != "" && response.lines == 1 {
		if err := dshResumeRejection(expectedSession, response.errorText); err != nil {
			return err
		}
	}
	if response.sessionID == "" {
		return errors.New("dsh 未提供原生会话 ID")
	}
	if exitCode != 0 {
		return fmt.Errorf("dsh 退出码 %d", exitCode)
	}
	return nil
}

// dshResumeRejection 只识别 0.2.1-rc.5 headless 在认领指定会话前输出的拒绝。
func dshResumeRejection(sessionID, message string) error {
	identity := `session "` + sessionID + `"`
	switch message {
	case identity + " is already owned by an active write handle",
		identity + " is live in this process, so the one-shot runner cannot own an exclusive run interval":
		return ErrSessionBusy
	case identity + " does not exist; omit --session-id to start a new Session":
		return ErrSessionUnavailable
	}
	if detail, ok := strings.CutPrefix(message, "stored "+identity+" is corrupt: "); ok && detail != "" {
		return ErrSessionUnavailable
	}
	if detail, ok := strings.CutPrefix(message, "failed to observe "+identity+": "+identity); ok && dshUnsupportedLogPattern.MatchString(detail) {
		return ErrSessionUnavailable
	}
	return nil
}
