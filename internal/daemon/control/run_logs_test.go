package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestRunLogRequestsUseOnlyDeviceAuthentication(t *testing.T) {
	identity := testIdentity()
	request := runreport.LogRequest{
		ID:    "log-1234567890abcdef1234567890abcdef",
		RunID: "run-11111111-2222-3333-4444-555555555555",
	}
	logs := runreport.LogResponse{Status: runreport.LogAvailable, Files: []runreport.LogFile{{Invocation: "cli-1", Stream: "stdout", Text: "fixture"}}}
	path := "/api/v1/devices/" + identity.DeviceID + "/run-log-requests"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+identity.Token || r.URL.RawQuery != "" {
			t.Error("日志接口须使用设备令牌认证")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == path:
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": []runreport.LogRequest{request}})
		case r.Method == http.MethodPut && r.URL.Path == path+"/"+request.ID:
			var received runreport.LogResponse
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil || !reflect.DeepEqual(received, logs) {
				t.Errorf("日志响应不匹配，解码错误 %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("日志接口路径不匹配 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	identity.HubURL = server.URL
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := client.RunLogRequests(t.Context(), identity)
	if err != nil || len(requests) != 1 || requests[0] != request {
		t.Fatalf("设备收到无效日志请求，数量 %d，错误 %v", len(requests), err)
	}
	if err := client.ReportRunLogs(t.Context(), identity, requests[0], logs); err != nil {
		t.Fatal(err)
	}
}
