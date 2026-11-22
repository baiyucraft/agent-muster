// Package helper 提供本机 muster 命令访问当前运行的进程间通信协议。
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const MaxPayload = 256 << 10

// Request 携带本机协作命令、运行能力凭据及命令对应的参数。
type Request struct {
	Capability     string   `json:"capability"`
	Command        string   `json:"command"`
	Key            string   `json:"key,omitempty"`
	Title          string   `json:"title,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	ProjectID      string   `json:"project_id,omitempty"`
	BindingID      string   `json:"binding_id,omitempty"`
	TargetDeviceID string   `json:"target_device_id,omitempty"`
	File           string   `json:"file,omitempty"`
	Message        string   `json:"message,omitempty"`
	TransferID     string   `json:"transfer_id,omitempty"`
	TaskIDs        []string `json:"task_ids,omitempty"`
	TransferIDs    []string `json:"transfer_ids,omitempty"`
}

type response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Listen 打开本机协作命令的系统通信端点。
func Listen() (net.Listener, error) { return listen() }

// Serve 为每条连接处理一个请求，上下文取消时关闭监听端点。
func Serve(ctx context.Context, listener net.Listener, handle func(context.Context, Request) (any, error)) {
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go serveConnection(ctx, connection, handle)
	}
}

func serveConnection(ctx context.Context, connection net.Conn, handle func(context.Context, Request) (any, error)) {
	defer connection.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	decoder := json.NewDecoder(io.LimitReader(connection, MaxPayload))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(response{Error: "本机命令格式无效"})
		return
	}
	result, err := handle(ctx, request)
	if err != nil {
		_ = json.NewEncoder(connection).Encode(response{Error: err.Error()})
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = json.NewEncoder(connection).Encode(response{Result: data})
}

// Call 在本机端点执行单次协作请求，返回原始 JSON 结果或 daemon 报告的错误。
func Call(ctx context.Context, address string, request Request) (json.RawMessage, error) {
	connection, err := dial(ctx, address)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, err
	}
	var reply response
	if err := json.NewDecoder(io.LimitReader(connection, MaxPayload)).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	return reply.Result, nil
}
