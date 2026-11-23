package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPForwardsRunScope 验证 MCP 发现、工具参数转发及 daemon 的能力校验。
func TestMCPForwardsRunScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := helper.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan helper.Request, 6)
	go helper.Serve(ctx, listener, func(_ context.Context, request helper.Request) (any, error) {
		if request.Capability != "current-turn" {
			return nil, errors.New("当前运行未给此命令授权")
		}
		requests <- request
		return map[string]string{"command": request.Command}, nil
	})
	connect := func(capability string) *mcp.ClientSession {
		t.Helper()
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		server, err := newMCPServer(listener.Addr().String(), capability).Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { server.Close() })
		client, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}
	client := connect("current-turn")
	listed, err := client.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 6 {
		t.Fatalf("预期发现六个工具，得到 %v，错误 %v", listed, err)
	}
	cases := []struct {
		arguments any
		request   helper.Request
	}{
		{struct{}{}, helper.Request{Command: "status"}},
		{delegateArgs{Key: "child", Title: "核对资料", Prompt: "阅读文档", DeviceID: "device", BindingID: "binding", ProjectID: "project", TransferIDs: []string{"file"}},
			helper.Request{Command: "delegate", Key: "child", Title: "核对资料", Prompt: "阅读文档", TargetDeviceID: "device", BindingID: "binding", ProjectID: "project", TransferIDs: []string{"file"}}},
		{sendArgs{Key: "deliver", DeviceID: "device", File: "out/report.txt"}, helper.Request{Command: "send", Key: "deliver", TargetDeviceID: "device", File: "out/report.txt"}},
		{receiveArgs{TransferID: "file"}, helper.Request{Command: "receive", TransferID: "file"}},
		{waitArgs{TaskIDs: []string{"task"}, TransferIDs: []string{"file"}}, helper.Request{Command: "wait", TaskIDs: []string{"task"}, TransferIDs: []string{"file"}}},
		{artifactArgs{File: "out/report.txt"}, helper.Request{Command: "artifact", File: "out/report.txt"}},
	}
	for _, item := range cases {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: item.request.Command, Arguments: item.arguments})
		if err != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s 工具调用失败，结果 %v，错误 %v", item.request.Command, result, err)
		}
		item.request.Capability = "current-turn"
		if got := <-requests; !reflect.DeepEqual(got, item.request) {
			t.Errorf("%s 转发参数为 %+v，预期 %+v", item.request.Command, got, item.request)
		}
	}
	for _, capability := range []string{"", "previous-turn"} {
		client := connect(capability)
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "status", Arguments: struct{}{}})
		if err != nil || !result.IsError {
			t.Fatalf("没有当前凭据时应返回工具错误，结果 %v，错误 %v", result, err)
		}
	}
}
