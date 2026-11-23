package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.ReadCloser, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, "用法: muster <status|delegate|send|receive|wait|artifact|mcp> [参数]，使用子命令 -h 查看参数")
		return err
	}
	if args[0] == "--version" || args[0] == "-version" {
		_, err := fmt.Fprintln(stdout, "muster dev")
		return err
	}
	if args[0] == "mcp" {
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			_, err := fmt.Fprintln(stdout, "用法: muster mcp（由 Agent 的 MCP stdio 客户端启动）")
			return err
		}
		if len(args) != 1 {
			return errors.New("muster mcp 不接受参数")
		}
		return serveMCP(stdin, stdout)
	}
	request := helper.Request{Command: args[0]}
	flags := flag.NewFlagSet(request.Command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	addTransfer := func(id string) error {
		request.TransferIDs = append(request.TransferIDs, id)
		return nil
	}
	switch request.Command {
	case "status":
	case "delegate", "spawn":
		flags.StringVar(&request.Key, "key", "", "幂等键")
		flags.StringVar(&request.Title, "title", "", "子任务标题")
		flags.StringVar(&request.Prompt, "prompt", "", "子任务提示词，使用 - 从标准输入读取")
		flags.StringVar(&request.ProjectID, "project", "", "目标项目 ID，默认当前项目")
		flags.StringVar(&request.BindingID, "binding", "", "目标绑定 ID")
		flags.StringVar(&request.TargetDeviceID, "device", "", "目标设备 ID")
		flags.Func("transfer", "需要投递的文件 ID，可重复", addTransfer)
	case "send":
		flags.StringVar(&request.Key, "key", "", "幂等键")
		flags.StringVar(&request.File, "file", "", "当前工作目录中的相对路径")
		flags.StringVar(&request.Message, "message", "", "要投递的文本消息（最多 64 KiB）")
		flags.StringVar(&request.TargetDeviceID, "device", "", "目标设备 ID")
	case "receive":
		flags.StringVar(&request.TransferID, "transfer", "", "本轮接收的投递 ID")
	case "artifact":
		flags.StringVar(&request.File, "file", "", "当前工作目录内要登记的相对文件路径")
	case "wait":
		flags.Func("task", "等待的子任务 ID，可重复", func(id string) error {
			request.TaskIDs = append(request.TaskIDs, id)
			return nil
		})
		flags.Func("transfer", "等待的投递 ID，可重复", addTransfer)
	default:
		return fmt.Errorf("未知 muster 命令 %q", request.Command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("请使用已命名的命令参数")
	}
	if request.Prompt == "-" {
		prompt, err := io.ReadAll(io.LimitReader(stdin, helper.MaxPayload+1))
		if err != nil {
			return err
		}
		if len(prompt) > helper.MaxPayload {
			return errors.New("提示词超过本机命令大小限制")
		}
		request.Prompt = strings.TrimSpace(string(prompt))
	}
	pipe := os.Getenv("MUSTER_PIPE")
	request.Capability = os.Getenv("MUSTER_RUN_CAPABILITY")
	if pipe == "" || request.Capability == "" {
		return errors.New("此命令需要在 Agent Muster 派发的运行中使用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := helper.Call(ctx, pipe, request)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(result))
	return err
}
