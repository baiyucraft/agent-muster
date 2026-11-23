package work

import "testing"

// TestParseGitCandidate 验证控制清单与公开结果分离，且拒绝含糊或额外的控制数据。
func TestParseGitCandidate(t *testing.T) {
	const result = "完成修复。"
	for _, item := range []struct {
		name   string
		footer string
		ok     bool
	}{
		{"选择文件", `{"message":"fix: 样例","files":["src/example.go"]}`, true},
		{"复用候选", `{"files":[],"message":""}`, true},
		{"缺少文件", `{"message":"fix: 样例"}`, false},
		{"文件为 null", `{"files":null,"message":""}`, false},
		{"说明为 null", `{"files":[],"message":null}`, false},
		{"空列表带提交说明", `{"files":[],"message":"fix: 样例"}`, false},
		{"重复字段", `{"files":["src/example.go"],"message":"一","message":"二"}`, false},
		{"附加标记", "{\"files\":[],\"message\":\"\"}\n" + gitCandidateMarker, false},
		{"附加字段", `{"files":[],"message":"","extra":1}`, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			body, manifest := parseGitCandidate(result + "\r\n" + gitCandidateMarker + "\r\n" + item.footer)
			if body != result || (manifest != nil) != item.ok {
				t.Fatalf("正文 %q，提交清单 %v，期望清单有效 %t", body, manifest, item.ok)
			}
		})
	}
}
