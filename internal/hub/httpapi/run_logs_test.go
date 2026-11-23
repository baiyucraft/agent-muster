package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

type testRunLogStore struct{ workspace.Store }

func (testRunLogStore) RunLogDevice(context.Context, string, string) (string, error) {
	return "device", nil
}

func TestRunLogReadDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	request.SetPathValue("id", "task-00000000-0000-4000-8000-000000000001")
	request.SetPathValue("run_id", "run-00000000-0000-4000-8000-000000000002")
	recorder := httptest.NewRecorder()
	api := runLogAPI{store: testRunLogStore{}, requests: make(map[string]*pendingRunLog)}
	api.read(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout || len(api.requests) != 0 {
		t.Fatalf("超时未返回 504 或未清理请求 (%d, %d)", recorder.Code, len(api.requests))
	}
}

func TestRunLogRequestLimits(t *testing.T) {
	api := runLogAPI{requests: make(map[string]*pendingRunLog)}
	for index := range maxRunLogs {
		pending := &pendingRunLog{
			request: runreport.LogRequest{ID: fmt.Sprint(index)},
			device:  fmt.Sprint(index / runreport.MaxLogRequests),
			ctx:     context.Background(),
		}
		if !api.enqueue(pending) {
			t.Fatalf("请求 %d 应入队", index)
		}
		if index == runreport.MaxLogRequests-1 && api.enqueue(&pendingRunLog{device: pending.device}) {
			t.Fatal("同一设备请求超过上限后仍可入队")
		}
	}
	if api.enqueue(&pendingRunLog{device: "other"}) {
		t.Fatal("所有设备请求超过上限后仍可入队")
	}
	pending, status := api.reserveUpload("0", "0")
	if status != http.StatusNoContent || pending == nil {
		t.Fatalf("请求无法接收设备回执，状态码 %d", status)
	}
	if _, status := api.reserveUpload("0", "0"); status != http.StatusTooManyRequests {
		t.Fatalf("同一请求允许并发上传，状态码 %d", status)
	}
}
