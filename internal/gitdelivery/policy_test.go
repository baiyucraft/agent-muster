package gitdelivery

import (
	"encoding/json"
	"strings"
	"testing"
)

func testPolicy() Policy {
	return Policy{
		BaseRef: "refs/remotes/origin/main",
		Remote:  "origin",
		Test:    Command{Program: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 180},
	}
}

func TestPolicyAcceptsLiteralReferencesAndArguments(t *testing.T) {
	policy := testPolicy()
	policy.Revision = 4
	policy.BaseRef = "refs/heads/团队/主线"
	policy.TargetBranch = "refs/heads/release/next"
	policy.Remote = "company-1.upstream"
	policy.Test = Command{
		Program: "npm.cmd", Args: []string{"test", "--", "", "with spaces", "a&b", `x"y`}, TimeoutSeconds: 3600,
	}
	policy.ReviewAgent = "codex"
	policy.MaxRepairs = 5
	if err := policy.Validate(); err != nil {
		t.Fatalf("字面交付策略应可保存: %v", err)
	}
	if err := testPolicy().Validate(); err != nil {
		t.Fatalf("缺省任务分支和零返修预算应可保存: %v", err)
	}
}

func TestPolicyRejectsInvalidDeliveryConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Policy)
	}{
		{"负版本", func(p *Policy) { p.Revision = -1 }},
		{"简写基线", func(p *Policy) { p.BaseRef = "main" }},
		{"标签基线", func(p *Policy) { p.BaseRef = "refs/tags/v1" }},
		{"缺少跟踪分支", func(p *Policy) { p.BaseRef = "refs/remotes/origin" }},
		{"空白基线", func(p *Policy) { p.BaseRef = "refs/heads/ release" }},
		{"前导横线远端", func(p *Policy) { p.Remote = "--all" }},
		{"远端 URL", func(p *Policy) { p.Remote = "https://example.test/repo.git" }},
		{"远端路径", func(p *Policy) { p.Remote = "../repo" }},
		{"引用锁远端", func(p *Policy) { p.Remote = "name.lock" }},
		{"空远端", func(p *Policy) { p.Remote = "" }},
		{"强推 refspec", func(p *Policy) { p.TargetBranch = "+refs/heads/main" }},
		{"推送 refspec", func(p *Policy) { p.TargetBranch = "refs/heads/dev:refs/heads/main" }},
		{"非分支目标", func(p *Policy) { p.TargetBranch = "refs/remotes/origin/main" }},
		{"提交祖先表达式", func(p *Policy) { p.TargetBranch = "refs/heads/main~2" }},
		{"reflog 表达式", func(p *Policy) { p.TargetBranch = "refs/heads/main@{1}" }},
		{"保留目录", func(p *Policy) { p.TargetBranch = "refs/heads/.git/main" }},
		{"引用锁目标", func(p *Policy) { p.TargetBranch = "refs/heads/main.lock" }},
		{"空目标分支", func(p *Policy) { p.TargetBranch = "refs/heads/" }},
		{"目标控制符", func(p *Policy) { p.TargetBranch = "refs/heads/main\x7f" }},
		{"目标反斜杠", func(p *Policy) { p.TargetBranch = `refs/heads/team\main` }},
		{"空程序", func(p *Policy) { p.Test.Program = "" }},
		{"带参数的程序", func(p *Policy) { p.Test.Program = "go test" }},
		{"相对程序路径", func(p *Policy) { p.Test.Program = `tools\test.exe` }},
		{"带句点别名", func(p *Policy) { p.Test.Program = "go." }},
		{"NUL 参数", func(p *Policy) { p.Test.Args = []string{"before\x00after"} }},
		{"无效 UTF-8 参数", func(p *Policy) { p.Test.Args = []string{"\xff"} }},
		{"零超时", func(p *Policy) { p.Test.TimeoutSeconds = 0 }},
		{"超出超时范围", func(p *Policy) { p.Test.TimeoutSeconds = 3601 }},
		{"未知审查 Agent", func(p *Policy) { p.ReviewAgent = "other" }},
		{"负返修预算", func(p *Policy) { p.MaxRepairs = -1 }},
		{"超出返修预算", func(p *Policy) { p.MaxRepairs = 6 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := testPolicy()
			tt.change(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatalf("无效交付策略通过校验: %+v", policy)
			}
		})
	}
}

func TestPolicyCloneOwnsArgumentsAndEncodesEmptyArray(t *testing.T) {
	original := testPolicy()
	frozen := original.Clone()
	original.Test.Args[1] = "./other/..."
	if frozen.Test.Args[1] != "./..." {
		t.Fatalf("冻结后的参数随策略配置变化: %v", frozen.Test.Args)
	}
	original.Test.Args = nil
	body, err := json.Marshal(original.Clone())
	if err != nil || !strings.Contains(string(body), `"args":[]`) {
		t.Fatalf("无参数命令应保存 JSON 空数组: %s, %v", body, err)
	}
}
