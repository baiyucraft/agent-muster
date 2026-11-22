// Package runner 探测本机 Agent 命令行工具并运行其原生会话。
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Capabilities 记录从帮助文本确认的提示词输入、最终回复和会话续接能力。
type Capabilities struct {
	Prompt     bool `json:"prompt"`
	FinalReply bool `json:"final_reply"`
	Resume     bool `json:"resume"`
}

// Agent 描述一种受探测 CLI 的安装状态、版本及已确认的调用能力。
type Agent struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	Status       string       `json:"status"`
	Capabilities Capabilities `json:"capabilities"`
}

var versionLine = regexp.MustCompile(`(?m)^(?:(?:codex-cli|dsh) )?v?([0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?(?:\+[a-zA-Z0-9.-]+)?)[ \t\r]*$`)

// Discover 并行探测 PATH 中的 Codex 和 dsh，仅运行有上限的版本与帮助命令。
func Discover(ctx context.Context) []Agent {
	ids := []string{"codex", "dsh"}
	agents := make([]Agent, len(ids))
	var probes sync.WaitGroup
	for index, id := range ids {
		probes.Go(func() {
			agents[index] = discoverAgent(ctx, id)
		})
	}
	probes.Wait()
	return agents
}

func discoverAgent(ctx context.Context, id string) Agent {
	agent := Agent{ID: id, Status: "probe_failed"}
	path, err := exec.LookPath(id)
	if errors.Is(err, exec.ErrNotFound) {
		agent.Status = "missing"
		return agent
	}
	if err != nil {
		return agent
	}

	version, err := inspectCommand(ctx, path, "--version")
	if err != nil {
		return agent
	}
	match := versionLine.FindStringSubmatch(version)
	if len(match) != 2 || len(match[1]) > 64 {
		return agent
	}
	agent.Version = match[1]

	switch id {
	case "codex":
		help, err := inspectCommand(ctx, path, "exec", "--help")
		if err != nil {
			return agent
		}
		agent.Capabilities.Prompt = strings.Contains(help, "read from stdin")
		agent.Capabilities.FinalReply = strings.Contains(help, "--output-last-message")
		if strings.Contains(help, "Resume a previous session by id") {
			resumeHelp, err := inspectCommand(ctx, path, "exec", "resume", "--help")
			if err != nil {
				return agent
			}
			agent.Capabilities.Resume = strings.Contains(resumeHelp, "[SESSION_ID]")
		}
	case "dsh":
		help, err := inspectCommand(ctx, path, "headless", "--help")
		if err != nil {
			return agent
		}
		agent.Capabilities = Capabilities{
			Prompt:     strings.Contains(help, "reads stdin"),
			FinalReply: strings.Contains(help, "answer goes to stdout"),
			Resume:     strings.Contains(help, "--session-id"),
		}
	}

	agent.Status = "unsupported"
	if agent.Capabilities.Prompt && agent.Capabilities.FinalReply && agent.Capabilities.Resume {
		agent.Status = "supported"
	}
	return agent
}

// Only fixed version and help arguments may be passed to this function.
func inspectCommand(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd, err := probeCommand(ctx, path, args...)
	if err != nil {
		return "", err
	}
	output := &boundedOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = time.Second
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, "MUSTER_DEVICE_TOKEN") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	err = cmd.Run()
	return string(output.data), err
}

type boundedOutput struct {
	data []byte
}

// Write 保留最多 32 KiB 探测输出，丢弃溢出部分并继续接收进程输出。
func (output *boundedOutput) Write(p []byte) (int, error) {
	const limit = 32 * 1024
	output.data = append(output.data, p[:min(len(p), limit-len(output.data))]...)
	return len(p), nil
}
