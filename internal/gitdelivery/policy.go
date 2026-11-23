// Package gitdelivery 定义项目 Git 交付策略及冻结执行所需的校验。
package gitdelivery

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	remoteName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	programName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
)

// CandidateCommitDaemon 允许设备端根据修复结果创建候选提交。
const CandidateCommitDaemon = "daemon"

// CandidateCommitHeader 供设备在领取运行时声明它支持创建候选提交。
const CandidateCommitHeader = "Muster-Git-Candidate-Commit"

// Policy 是管理员保存并在交付任务首次派发时冻结的项目设置。
type Policy struct {
	// Revision 由 Hub 写入。未保存的配置可使用零值。
	Revision int64 `json:"revision"`
	// BaseRef 是设备本地用于确定修复基线的分支或远端跟踪引用。
	BaseRef string `json:"base_ref"`
	// Remote 是设备仓库中已配置的远端别名。
	Remote string `json:"remote"`
	// TargetBranch 是完整的目标分支引用。空值使用由任务 ID 确定的分支。
	TargetBranch string  `json:"target_branch,omitempty"`
	Test         Command `json:"test"`
	// CandidateCommit 为空时由修复 Agent 自行创建候选提交。
	CandidateCommit string `json:"candidate_commit,omitempty"`
	// ReviewAgent 为空时仅执行测试。dsh 或 codex 表示另开该 CLI 的审查会话。
	ReviewAgent string `json:"review_agent,omitempty"`
	// MaxRepairs 是首个候选验证失败后允许的额外修复次数。
	MaxRepairs int `json:"max_repairs"`
}

// Command 保存经可执行文件搜索路径解析的测试程序及逐项传给程序的参数。
type Command struct {
	Program string `json:"program"`
	// Args 按原序传递。空字符串和空格是参数的一部分。
	Args []string `json:"args"`
	// TimeoutSeconds 是单次测试的运行时间上限，单位为秒。
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Validate 校验可持久化的策略字段。设备执行前还需核对仓库、远端和程序。
func (policy Policy) Validate() error {
	if policy.Revision < 0 {
		return errors.New("Git 交付策略版本不能为负数")
	}
	if !validBaseRef(policy.BaseRef) {
		return errors.New("Git 交付基线须为完整的本地分支或远端跟踪引用")
	}
	if !remoteName.MatchString(policy.Remote) || !validRef("refs/remotes/"+policy.Remote+"/main") {
		return errors.New("Git 交付远端须为仓库配置中的别名")
	}
	if policy.TargetBranch != "" && !validBranchRef(policy.TargetBranch) {
		return errors.New("Git 交付目标须为完整的 refs/heads/ 分支引用")
	}
	if policy.CandidateCommit != "" && policy.CandidateCommit != CandidateCommitDaemon {
		return errors.New("Git 交付候选提交方式须为空或 daemon")
	}
	if !programName.MatchString(policy.Test.Program) || strings.HasSuffix(policy.Test.Program, ".") {
		return errors.New("Git 交付测试程序须为可执行文件名")
	}
	for i, arg := range policy.Test.Args {
		if !utf8.ValidString(arg) || strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("Git 交付测试的第 %d 个参数须为无 NUL 的 UTF-8 文本", i+1)
		}
	}
	if policy.Test.TimeoutSeconds < 1 || policy.Test.TimeoutSeconds > 3600 {
		return errors.New("Git 交付测试超时须在 1 至 3600 秒之间")
	}
	if policy.ReviewAgent != "" && policy.ReviewAgent != "dsh" && policy.ReviewAgent != "codex" {
		return errors.New("Git 交付审查 Agent 须为 dsh 或 codex")
	}
	if policy.MaxRepairs < 0 || policy.MaxRepairs > 5 {
		return errors.New("Git 交付返修次数须在 0 至 5 次之间")
	}
	return nil
}

// Clone 返回独立拥有参数切片的策略副本，空参数列表序列化为 JSON 数组。
func (policy Policy) Clone() Policy {
	args := make([]string, len(policy.Test.Args))
	copy(args, policy.Test.Args)
	policy.Test.Args = args
	return policy
}

func validBaseRef(ref string) bool {
	if branch, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		// 远端跟踪引用须同时包含远端和分支两段。
		_, _, hasBranch := strings.Cut(branch, "/")
		return hasBranch && validRef(ref)
	}
	return validBranchRef(ref)
}

func validBranchRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") && validRef(ref)
}

// validRef 在 Hub 上按 Git 的引用名称规则检查字面引用，避免接受修订表达式或 refspec。
func validRef(ref string) bool {
	if !utf8.ValidString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".") ||
		strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for _, char := range ref {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
