package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
)

type runEventCapture struct {
	workspace.Store
	event workspace.RunEvent
}

func (capture *runEventCapture) RecordRunEvent(_ context.Context, _ string, _ [32]byte, _ string, _ int64, event workspace.RunEvent) error {
	capture.event = event
	return nil
}

func TestRunEventAcceptsEscapedTextAndInputReceipt(t *testing.T) {
	store := &runEventCapture{}
	api := workspaceAPI{store: store}
	ids := make([]string, 320)
	for i := range ids {
		ids[i] = fmt.Sprintf("message:00000000-0000-4000-8000-%012d", i)
	}
	event := workspace.RunEvent{Kind: "completed", Text: strings.Repeat("<", 12000), InputReceipt: &inputreceipt.Receipt{HandledIDs: ids}}
	body, err := json.Marshal(event)
	if err != nil || len(body) <= 80<<10 {
		t.Fatalf("测试请求应超过原有上限，当前为 %d 字节，错误 %v", len(body), err)
	}
	request := func(payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "run-00000000-0000-4000-8000-000000000001")
		req.SetPathValue("sequence", "1")
		response := httptest.NewRecorder()
		api.recordEvent(response, req, "fixture", [32]byte{})
		return response
	}
	response := request(string(body))
	if response.Code != http.StatusNoContent || store.event.Text != event.Text || len(store.event.InputReceipt.HandledIDs) != len(ids) {
		t.Fatalf("运行回执未完整接收，HTTP %d，响应 %s", response.Code, response.Body)
	}
	if response := request(strings.Repeat(" ", 256<<10) + string(body)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限运行回执 HTTP %d，响应 %s", response.Code, response.Body)
	}
}
