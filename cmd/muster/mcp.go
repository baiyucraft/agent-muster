package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type delegateArgs struct {
	Key         string   `json:"key" jsonschema:"同一个子任务请求重试时使用的稳定键"`
	Title       string   `json:"title"`
	Prompt      string   `json:"prompt"`
	DeviceID    string   `json:"device_id"`
	BindingID   string   `json:"binding_id"`
	ProjectID   string   `json:"project_id,omitempty" jsonschema:"缺省时使用本轮项目"`
	TransferIDs []string `json:"transfer_ids,omitempty" jsonschema:"子任务需要的已发送投递 ID"`
}

type sendArgs struct {
	Key      string `json:"key" jsonschema:"同一次投递重试时使用的稳定键"`
	DeviceID string `json:"device_id"`
	File     string `json:"file,omitempty" jsonschema:"工作目录内的相对文件路径，与 message 二选一"`
	Message  string `json:"message,omitempty" jsonschema:"最多 64 KiB 的文本，与 file 二选一"`
}

type receiveArgs struct {
	TransferID string `json:"transfer_id"`
}

type waitArgs struct {
	TaskIDs     []string `json:"task_ids,omitempty" jsonschema:"等待的子任务 ID"`
	TransferIDs []string `json:"transfer_ids,omitempty" jsonschema:"等待的投递 ID，与 task_ids 至少填写一项"`
}

type artifactArgs struct {
	File string `json:"file" jsonschema:"工作目录内的相对文件路径"`
}

// serveMCP 通过标准输入输出提供一条会话，客户端关闭输入后退出。
func serveMCP(stdin io.ReadCloser, stdout io.Writer) error {
	transport := &mcp.IOTransport{
		Reader: stdin, Writer: stdioOutput{stdout}, MaxLineLength: helper.MaxPayload + 8192,
	}
	return newMCPServer(os.Getenv("MUSTER_PIPE"), os.Getenv("MUSTER_RUN_CAPABILITY")).Run(context.Background(), transport)
}

type stdioOutput struct{ io.Writer }

// Close 保留进程对标准输出的所有权，MCP 关闭会话后由调用方收尾。
func (stdioOutput) Close() error { return nil }

// newMCPServer 只把已声明的协作工具转发给本机 daemon，能力凭据取自启动环境。
func newMCPServer(pipe, capability string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: "1"}, nil)
	run := func(ctx context.Context, request helper.Request) (*mcp.CallToolResult, any, error) {
		if pipe == "" || capability == "" {
			return nil, nil, errors.New("此工具需要在 Agent Muster 派发的运行中使用")
		}
		request.Capability = capability
		payload, err := json.Marshal(request)
		if err != nil {
			return nil, nil, err
		}
		if len(payload)+1 > helper.MaxPayload {
			return nil, nil, errors.New("参数超过本机命令大小限制")
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := helper.Call(ctx, pipe, request)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(result)}}}, nil, nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: "status", Description: "查看当前运行、已收到的材料及可用协作目标", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "status"})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "delegate", Description: "从当前运行派发子任务。先用 status 查目标设备和项目绑定"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args delegateArgs) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "delegate", Key: args.Key, Title: args.Title, Prompt: args.Prompt,
				TargetDeviceID: args.DeviceID, ProjectID: args.ProjectID, BindingID: args.BindingID, TransferIDs: args.TransferIDs})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "send", Description: "向设备交接文件或文本消息"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "send", Key: args.Key, TargetDeviceID: args.DeviceID, File: args.File, Message: args.Message})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "receive", Description: "导出本轮收到的附件并返回工作目录内的相对路径"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args receiveArgs) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "receive", TransferID: args.TransferID})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "wait", Description: "登记续接前需要等待的子任务或投递。登记成功后应结束本轮以释放设备"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args waitArgs) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "wait", TaskIDs: args.TaskIDs, TransferIDs: args.TransferIDs})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "artifact", Description: "将已写完并关闭的本轮工作文件登记为任务产物"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args artifactArgs) (*mcp.CallToolResult, any, error) {
			return run(ctx, helper.Request{Command: "artifact", File: args.File})
		})
	return server
}
