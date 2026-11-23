package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/artifact"
)

const (
	maxCommitPaths   = 256
	maxCommitMessage = 4096
	maxCommitOutput  = 256 << 10
	commitInbox      = ".muster-inbox"
)

// FixHead 读取已登记的 detached 修复工作树 HEAD，并核对它是冻结基线或最近一次受控提交。
// 上一次提交结果未确认时，只有与原清单一致的 HEAD 才能用于后续轮次。
func (work Workspace) FixHead(ctx context.Context) (string, error) {
	head, err := work.readFixHead(ctx)
	if err != nil {
		return "", err
	}
	if err := work.reconcileCommit(ctx, head); err != nil {
		return "", err
	}
	return head, nil
}

func (work Workspace) readFixHead(ctx context.Context) (string, error) {
	if !validOID(work.BaseOID) || !filepath.IsAbs(work.FixDir) {
		return "", errors.New("Git 修复工作区尚未初始化")
	}
	if err := work.verifyWorktree(ctx, work.FixDir); err != nil {
		return "", err
	}
	head, err := worktreeHead(ctx, work.FixDir)
	if err != nil || head == work.BaseOID {
		return head, err
	}
	_, code, err := runGit(ctx, work.FixDir, "merge-base", "--is-ancestor", work.BaseOID, head)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", errors.New("Git 修复工作区 HEAD 未由冻结基线派生")
	}
	return head, nil
}

// CommitSelected 在修复会话退出后，按清单提交当前工作树的普通文件变更。
// 清单须逐项匹配 Git 所见的变更，且 HEAD 须与会话启动前记录的一致。
// 提交使用仓库的身份、签名和钩子。失败时保留工作文件和设备本地提交日志。
func (work Workspace) CommitSelected(ctx context.Context, expectedHead string, paths []string, message string) (string, error) {
	if len(message) > maxCommitMessage || !utf8.ValidString(message) || strings.ContainsRune(message, 0) || strings.TrimSpace(message) == "" {
		return "", errors.New("Git 提交说明须为非空且不超过 4096 字节的 UTF-8 文本")
	}
	selected, err := commitSelection(paths)
	if err != nil {
		return "", err
	}
	if head, err := work.FixHead(ctx); err != nil {
		return "", err
	} else if head != expectedHead {
		return "", errors.New("Git 修复工作区 HEAD 已变化")
	}
	status, err := gitWithOutputLimit(ctx, work.FixDir, maxCommitOutput, "--no-optional-locks", "-c", "core.fsmonitor=false", "status",
		"--porcelain=v2", "-z", "--no-renames", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	newFiles, err := inspectCommitChanges(work.FixDir, status, selected)
	if err != nil {
		return "", err
	}
	pathspec, err := writeCommitPathspec(work.taskDir, paths)
	if err != nil {
		return "", err
	}
	defer os.Remove(pathspec)
	// 先保存清单和父提交。若进程中断或钩子改变提交范围，恢复时仍能核对本次写入。
	if err := work.recordCommit(commitRecord{Parent: expectedHead, Paths: paths}); err != nil {
		return "", err
	}
	if len(newFiles) != 0 {
		newPathspec, err := writeCommitPathspec(work.taskDir, newFiles)
		if err != nil {
			return "", err
		}
		defer os.Remove(newPathspec)
		// 新文件只登记加入意图。钩子拒绝提交后仍可用同一清单重试。
		if _, err := git(ctx, work.FixDir, "--literal-pathspecs", "-c", "core.fsmonitor=false", "add", "--intent-to-add",
			"--pathspec-from-file="+newPathspec, "--pathspec-file-nul"); err != nil {
			return "", fmt.Errorf("无法登记 Git 新文件: %w", err)
		}
	}
	if head, err := work.readFixHead(ctx); err != nil {
		return "", err
	} else if head != expectedHead {
		return "", errors.New("Git 修复工作区 HEAD 已变化")
	}
	if err := work.runCandidateCommit(ctx, pathspec, message); err != nil {
		return "", err
	}
	current, err := work.readFixHead(ctx)
	if err != nil {
		return "", err
	}
	if err := work.verifySelectedCommit(ctx, expectedHead, current, selected); err != nil {
		return "", err
	}
	return current, work.acceptCommit(current)
}

// commitSelection 校验清单字段，并保留 Git 状态需要的精确路径拼写。
func commitSelection(paths []string) (map[string]bool, error) {
	if len(paths) == 0 || len(paths) > maxCommitPaths {
		return nil, errors.New("Git 候选文件清单须包含 1 至 256 项")
	}
	selected := make(map[string]bool, len(paths))
	folded := make(map[string]bool, len(paths))
	for _, path := range paths {
		key := strings.ToLower(path)
		if !artifact.ValidPath(path) || !filepath.IsLocal(filepath.FromSlash(path)) || isCommitInbox(path) {
			return nil, fmt.Errorf("Git 候选清单中的文件路径无效 %q", path)
		}
		if folded[key] {
			return nil, errors.New("Git 候选清单包含重复路径或仅大小写不同的重命名")
		}
		for component := range strings.SplitSeq(path, "/") {
			if strings.EqualFold(component, ".git") {
				return nil, errors.New("Git 候选清单包含 Git 元数据路径")
			}
		}
		selected[path] = true
		folded[key] = true
	}
	return selected, nil
}

// inspectCommitChanges 使用 Git 机器可读的状态匹配清单。已有暂存、冲突和子模块变更须先解决。
func inspectCommitChanges(dir, status string, selected map[string]bool) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var newFiles []string
	matched := 0
	for record := range strings.SplitSeq(strings.TrimSuffix(status, "\x00"), "\x00") {
		if record == "" && status == "" {
			break
		}
		name, deleted, untracked, err := commitStatusFile(record)
		if err != nil {
			return nil, err
		}
		if untracked && isCommitInbox(name) {
			continue
		}
		if !selected[name] {
			return nil, fmt.Errorf("Git 修复工作区有不在候选清单中的变更 %q", name)
		}
		if err := checkCommitPath(root, name, deleted); err != nil {
			return nil, err
		}
		matched++
		if untracked {
			newFiles = append(newFiles, name)
		}
	}
	if matched != len(selected) {
		return nil, errors.New("Git 候选清单包含无变更、已忽略或不存在的文件")
	}
	return newFiles, nil
}

// commitStatusFile 读取 porcelain v2 普通文件记录，区分新文件、待提交删除和 Git 加入意图。
func commitStatusFile(record string) (name string, deleted, untracked bool, err error) {
	if name, ok := strings.CutPrefix(record, "? "); ok && name != "" {
		return name, false, true, nil
	}
	fields := strings.SplitN(record, " ", 9)
	if len(fields) != 9 || fields[0] != "1" || fields[2] != "N..." {
		return "", false, false, errors.New("Git 修复工作区包含冲突、子模块或异常状态")
	}
	for _, mode := range fields[3:6] {
		if !regularCommitMode(mode) {
			return "", false, false, errors.New("Git 候选仅支持普通文件")
		}
	}
	switch fields[1] {
	case ".M", ".A":
		return fields[8], false, false, nil
	case ".D":
		return fields[8], true, false, nil
	default:
		return "", false, false, errors.New("Git 修复工作区已有暂存变更或不支持的文件变更")
	}
}

func regularCommitMode(mode string) bool {
	return mode == "000000" || mode == "100644" || mode == "100755"
}

func isCommitInbox(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	return strings.EqualFold(first, commitInbox)
}

// checkCommitPath 拒绝工作树中的非普通文件及路径组件中的重解析点。删除允许路径已不存在。
func checkCommitPath(root *os.Root, name string, deleted bool) error {
	var path string
	for component := range strings.SplitSeq(name, "/") {
		path = filepath.Join(path, component)
		info, err := root.Lstat(path)
		if deleted && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		last := path == filepath.FromSlash(name)
		if err != nil || hasCommitReparsePoint(info) || (last && (!info.Mode().IsRegular() || deleted)) || (!last && !info.IsDir()) {
			return fmt.Errorf("Git 候选路径须指向工作区内的普通文件 %q", name)
		}
	}
	return nil
}

// writeCommitPathspec 在工作树外保存 NUL 分隔的字面路径，避免 Windows 命令行长度限制。
func writeCommitPathspec(dir string, paths []string) (string, error) {
	file, err := os.CreateTemp(dir, "commit-paths-*")
	if err != nil {
		return "", err
	}
	_, writeErr := file.WriteString(strings.Join(paths, "\x00") + "\x00")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// verifySelectedCommit 确认钩子执行后的候选仍是该轮基线上的单亲提交，且只含清单中的文件。
func (work Workspace) verifySelectedCommit(ctx context.Context, expectedHead, current string, selected map[string]bool) error {
	parent, err := git(ctx, work.FixDir, "rev-list", "--parents", "-1", current)
	if err != nil {
		return err
	}
	if current == expectedHead || strings.TrimSpace(parent) != current+" "+expectedHead {
		return errors.New("Git 候选提交的直接父提交与预期不符")
	}
	changed, err := gitWithOutputLimit(ctx, work.FixDir, maxCommitOutput, "diff-tree", "--no-commit-id", "--raw", "-r", "-z", "--no-renames", expectedHead, current)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00")
	if len(fields) != len(selected)*2 {
		return errors.New("Git 候选提交的文件与清单不匹配")
	}
	seen := make(map[string]bool, len(selected))
	for index := 0; index < len(fields); index += 2 {
		modes := strings.Fields(fields[index])
		path := fields[index+1]
		if len(modes) != 5 || !regularCommitMode(strings.TrimPrefix(modes[0], ":")) || !regularCommitMode(modes[1]) || !selected[path] || seen[path] {
			return errors.New("Git 候选提交包含清单外或不支持的文件变更")
		}
		seen[path] = true
	}
	return nil
}
