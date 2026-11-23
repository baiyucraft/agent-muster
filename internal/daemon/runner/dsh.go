package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// ErrProcessExitUnconfirmed 表示 Windows 未确认本轮所属进程树已全部退出。
var ErrProcessExitUnconfirmed = errors.New("无法确认本轮 Agent 进程树已退出")

// RunDSH 从标准输入向无界面的 dsh 传递提示词，并在解析到原生会话事件后回调 session。
func RunDSH(ctx context.Context, directory, prompt, sessionID string, started func() error, session func(string) error) (string, error) {
	return RunDSHWithEnvironment(ctx, directory, prompt, sessionID, nil, started, session)
}

// RunDSHWithEnvironment 在项目目录运行 dsh，并向命令进程注入许可的本机协作环境变量。
func RunDSHWithEnvironment(ctx context.Context, directory, prompt, sessionID string, env []string, started func() error, session func(string) error) (string, error) {
	path, err := exec.LookPath("dsh")
	if err != nil {
		return "", errors.New("无法启动 dsh，请检查本机安装和 PATH")
	}
	args := []string{"--profile", "headless", "--json"}
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
	var final string
	exitCode, err := runInJob(ctx, cmd, prompt, started, func(stdout io.Reader) error {
		var parseErr error
		final, parseErr = readDSH(stdout, sessionID, session)
		return parseErr
	})
	if err != nil {
		return final, err
	}
	if exitCode != 0 {
		return final, fmt.Errorf("dsh 退出码 %d", exitCode)
	}
	if strings.TrimSpace(final) == "" {
		return "", errors.New("dsh 未提供最终回复")
	}
	return final, nil
}

func readDSH(stdout io.Reader, expectedSession string, session func(string) error) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 8192), 4<<20)
	var actualSession, final string
	for scanner.Scan() {
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			// The stream may include local startup diagnostics. Only typed
			// session and final packets can change the execution result.
			continue
		}
		switch event.Type {
		case "session":
			if !sessionIDPattern.MatchString(event.SessionID) || expectedSession != "" && event.SessionID != expectedSession {
				return final, errors.New("dsh 返回了不匹配的会话 ID")
			}
			if actualSession != "" {
				if actualSession != event.SessionID {
					return final, errors.New("dsh 在同一轮发出了多个会话 ID")
				}
				continue
			}
			if err := session(event.SessionID); err != nil {
				return final, fmt.Errorf("无法保存 dsh 会话: %w", err)
			}
			actualSession = event.SessionID
		case "final":
			final = event.Text
		}
	}
	if err := scanner.Err(); err != nil {
		return final, fmt.Errorf("无法读取 dsh 输出: %w", err)
	}
	if actualSession == "" {
		return final, errors.New("dsh 未提供原生会话 ID")
	}
	return final, nil
}
