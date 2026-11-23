package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestProjectGitPolicyLifecycle(t *testing.T) {
	_, _, _, _, do := openWorkspaceFixture(t)
	var pairing struct{ Code string }
	do(http.MethodPost, "pairing-codes", testAdmin, map[string]string{"name": "开发设备"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do(http.MethodPost, "devices/enroll", "", map[string]string{
		"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64",
	}, 201, &device)
	dir := "dir-" + strings.Repeat("b", 32)
	do(http.MethodPut, "devices/"+device.ID+"/directories", device.Token,
		map[string]any{"directories": []workspace.Directory{{ID: dir, Label: "Git 项目"}}}, 204, nil)
	path := "projects/00000000-0000-4000-8000-000000000002"
	input := map[string]any{"name": "测试项目", "device_id": device.ID, "directory_id": dir}
	policy := map[string]any{
		"base_ref": "refs/heads/main", "remote": "origin", "max_repairs": 1,
		"test": map[string]any{"program": "go", "args": []string{"test", "./..."}, "timeout_seconds": 30},
	}
	input["git_policy"] = policy
	var project workspace.Project
	do(http.MethodPut, path, device.Token, input, 401, nil)
	do(http.MethodPut, path, testPlugin, input, 401, nil)
	do(http.MethodPut, path, testAdmin, input, 200, &project)
	if project.GitPolicy == nil || project.GitPolicy.Revision != 1 || project.GitPolicy.Test.Args[1] != "./..." {
		t.Fatalf("项目测试规则保存不完整：%+v", project.GitPolicy)
	}
	do(http.MethodPut, path, testAdmin, input, 200, &project)
	if project.GitPolicy == nil || project.GitPolicy.Revision != 1 {
		t.Fatalf("重复保存改变了版本：%+v", project.GitPolicy)
	}
	delete(input, "git_policy")
	input["name"] = "旧版编辑器改名"
	do(http.MethodPut, path, testAdmin, input, 200, &project)
	if project.Name != input["name"] || project.GitPolicy == nil || project.GitPolicy.Revision != 1 {
		t.Fatalf("旧请求未保留规则：%+v", project)
	}
	policy["target_branch"] = "main"
	input["git_policy"] = policy
	do(http.MethodPut, path, testAdmin, input, 400, nil)
	input["git_policy"] = nil
	project = workspace.Project{}
	do(http.MethodPut, path, testAdmin, input, 200, &project)
	if project.GitPolicy != nil {
		t.Fatal("显式清空后仍返回旧规则")
	}
	delete(policy, "target_branch")
	input["git_policy"] = policy
	do(http.MethodPut, path, testAdmin, input, 200, &project)
	if project.GitPolicy == nil || project.GitPolicy.Revision != 3 {
		t.Fatalf("重新启用时版本未递增：%+v", project.GitPolicy)
	}
}
