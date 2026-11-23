package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestArtifactDownloadThroughDevice(t *testing.T) {
	ctx, _, _, server, do := openWorkspaceFixture(t)
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(name string) control.Identity {
		t.Helper()
		var pairing struct{ Code string }
		do(http.MethodPost, "pairing-codes", testAdmin, map[string]string{"name": name}, 201, &pairing)
		identity := control.Identity{HubURL: server.URL}
		do(http.MethodPost, "devices/enroll", "", map[string]string{
			"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64",
		}, 201, &identity)
		return identity
	}
	owner, other := enroll("产物设备"), enroll("另一台设备")
	const project = "00000000-0000-4000-8000-000000000001"
	directory := "dir-" + strings.Repeat("a", 32)
	do(http.MethodPut, "devices/"+owner.DeviceID+"/directories", owner.Token, map[string]any{
		"directories": []workspace.Directory{{ID: directory, Label: "素材目录"}},
	}, 204, nil)
	do(http.MethodPut, "projects/"+project, testAdmin, map[string]string{
		"name": "素材目录", "device_id": owner.DeviceID, "directory_id": directory,
	}, 200, nil)
	do(http.MethodPost, "conversation/messages", testAdmin, map[string]string{
		"id": "00000000-0000-4000-8000-000000000002", "text": "生成索引",
	}, 202, nil)
	var delivery workspace.Delivery
	do(http.MethodPost, "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	do(http.MethodPost, "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: project, Title: "生成索引", Prompt: "生成索引",
	}, 200, nil)
	run, err := client.ClaimRun(ctx, owner)
	if err != nil || run == nil || !run.ArtifactsSupported {
		t.Fatalf("领取未启用设备产物: %+v, %v", run, err)
	}
	body := []byte("名称,数量\n物料A,2\n")
	sum := sha256.Sum256(body)
	ref := artifact.Reference{Path: "output/物料.csv", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	ref.ID = artifact.ID(run.ID, ref.Path, ref.SHA256)
	event := workspace.RunEvent{Kind: "started", Lease: run.Lease, Generation: run.Generation, SessionID: "native"}
	eventPath := "devices/" + owner.DeviceID + "/runs/" + run.ID + "/events/"
	do(http.MethodPut, eventPath+"1", owner.Token, event, 204, nil)
	event.Kind, event.Text, event.Artifacts = "completed", "索引已交付", []artifact.Reference{ref}
	do(http.MethodPut, eventPath+"2", owner.Token, event, 204, nil)
	var detail workspace.Detail
	do(http.MethodGet, "tasks/"+run.TaskID, testAdmin, nil, 200, &detail)
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Reference != ref || detail.Artifacts[0].DeviceID != owner.DeviceID {
		t.Fatalf("任务详情产物与本轮不匹配: %+v", detail.Artifacts)
	}
	path := "runs/" + run.ID + "/artifacts/" + ref.ID
	do(http.MethodGet, path, owner.Token, nil, 401, nil)
	do(http.MethodGet, path, testPlugin, nil, 401, nil)
	do(http.MethodHead, path, testAdmin, nil, 405, nil)
	do(http.MethodGet, "runs/"+run.ID+"/artifacts/"+strings.Repeat("a", 64), testAdmin, nil, 404, nil)
	do(http.MethodGet, path, testAdmin, nil, 409, nil)
	do(http.MethodPost, "devices/"+owner.DeviceID+"/heartbeat", owner.Token, nil, 204, nil)

	type download struct {
		response *http.Response
		err      error
	}
	for _, tc := range []struct {
		status string
		body   []byte
		code   int
	}{{"available", body, 200}, {"available", []byte("corrupted"), 502}, {"missing", nil, 404}} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+testAdmin)
		ready := make(chan download, 1)
		go func() {
			result, err := server.Client().Do(request)
			ready <- download{result, err}
		}()
		var queued artifact.ReadRequest
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			queued, err = client.ArtifactRead(ctx, owner)
			if err != nil || queued.ID != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil || queued.RunID != run.ID || queued.ArtifactID != ref.ID {
			t.Fatalf("设备未领取指定产物: %+v, %v", queued, err)
		}
		do(http.MethodGet, path, testAdmin, nil, 429, nil)
		if result, err := client.ArtifactRead(ctx, other); err != nil || result.ID != "" {
			t.Fatalf("无关设备收到了产物请求: %+v, %v", result, err)
		}
		var status *control.HTTPError
		if err := client.ReportArtifact(ctx, other, queued.ID, "available", body); !errors.As(err, &status) || status.Status != 404 {
			t.Fatalf("无关设备可以提交正文: %v", err)
		}
		if err := client.ReportArtifact(ctx, owner, queued.ID, tc.status, tc.body); err != nil {
			t.Fatal(err)
		}
		result := <-ready
		if result.err != nil {
			t.Fatal(result.err)
		}
		content, err := io.ReadAll(result.response.Body)
		result.response.Body.Close()
		if err != nil || result.response.StatusCode != tc.code {
			t.Fatalf("产物响应状态=%d 希望=%d: %v", result.response.StatusCode, tc.code, err)
		}
		if tc.code == 200 {
			kind, params, err := mime.ParseMediaType(result.response.Header.Get("Content-Disposition"))
			if err != nil || kind != "attachment" || params["filename"] != "物料.csv" || !bytes.Equal(content, body) ||
				result.response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("下载响应或正文不匹配: %v", result.response.Header)
			}
		}
		if err := client.ReportArtifact(ctx, owner, queued.ID, tc.status, tc.body); !errors.As(err, &status) || status.Status != 404 {
			t.Fatalf("结束请求仍接受旧回执: %v", err)
		}
	}
}
