// Package gitwork 管理设备上的 Git 修复工作区及绑定提交的测试证据。
package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

// Workspace 记录任务的固定基线和专用修复目录。
type Workspace struct {
	BaseOID string
	FixDir  string

	root    string
	common  string
	taskDir string
	test    gitdelivery.Command
}

// Prepare 在独占的设备任务目录中创建 detached 修复工作区。绑定目录须为 Git 工作区根，
// 任务目录须与绑定目录和 Git 数据目录分离。恢复时沿用首次保存的基线并保留原有修改。
func Prepare(ctx context.Context, boundDirectory, taskDirectory string, policy gitdelivery.Policy) (Workspace, error) {
	if err := policy.Validate(); err != nil {
		return Workspace{}, err
	}
	if !filepath.IsAbs(boundDirectory) || !filepath.IsAbs(taskDirectory) {
		return Workspace{}, errors.New("Git 绑定目录和任务目录须为绝对路径")
	}
	root, err := filepath.EvalSymlinks(boundDirectory)
	if err != nil {
		return Workspace{}, fmt.Errorf("无法打开 Git 绑定目录: %w", err)
	}
	if err := verifyTopLevel(ctx, root); err != nil {
		return Workspace{}, err
	}
	common, err := gitCommonDirectory(ctx, root)
	if err != nil {
		return Workspace{}, err
	}
	taskDir, err := resolveTaskDirectory(filepath.Clean(taskDirectory))
	if err != nil {
		return Workspace{}, fmt.Errorf("无法定位 Git 任务目录: %w", err)
	}
	if overlaps(root, taskDir) || overlaps(common, taskDir) {
		return Workspace{}, errors.New("Git 任务目录须与绑定仓库路径分离")
	}
	if err := os.MkdirAll(taskDir, 0700); err != nil {
		return Workspace{}, err
	}
	work := Workspace{root: root, common: common, taskDir: taskDir, FixDir: filepath.Join(taskDir, "fix"), test: policy.Clone().Test}
	work.BaseOID, err = work.loadBase(ctx, policy.BaseRef)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := os.Lstat(work.FixDir); errors.Is(err, os.ErrNotExist) {
		if err := work.addWorktree(ctx, work.FixDir, work.BaseOID); err != nil {
			return Workspace{}, fmt.Errorf("无法创建 Git 修复工作区: %w", err)
		}
	} else if err != nil {
		return Workspace{}, err
	}
	if err := work.verifyWorktree(ctx, work.FixDir); err != nil {
		return Workspace{}, err
	}
	return work, nil
}

// Candidate 返回干净修复工作区中完成提交的 HEAD。它须为冻结基线的后代。
func (work Workspace) Candidate(ctx context.Context) (string, error) {
	if !validOID(work.BaseOID) || !filepath.IsAbs(work.FixDir) {
		return "", errors.New("Git 修复工作区尚未初始化")
	}
	if err := work.verifyWorktree(ctx, work.FixDir); err != nil {
		return "", err
	}
	if err := requireClean(ctx, work.FixDir, false); err != nil {
		return "", err
	}
	untracked, err := git(ctx, work.FixDir, "ls-files", "--others", "--exclude-standard", "--",
		".", ":(top,exclude,icase).muster-inbox")
	if err != nil {
		return "", err
	}
	if untracked != "" {
		return "", errors.New("Git 修复工作区仍有未跟踪的项目文件")
	}
	oid, err := worktreeHead(ctx, work.FixDir)
	if err != nil {
		return "", err
	}
	if oid == work.BaseOID {
		return "", errors.New("Git 修复工作区还没有基线之后的提交")
	}
	_, code, err := runGit(ctx, work.FixDir, "merge-base", "--is-ancestor", work.BaseOID, oid)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("无法确认 Git 候选由冻结基线派生（退出码 %d）", code)
	}
	inboxHistory, err := git(ctx, work.FixDir, "log", "--full-history", "-1", "--format=%H",
		work.BaseOID+".."+oid, "--", ":(top,icase).muster-inbox")
	if err != nil {
		return "", err
	}
	if inboxHistory != "" {
		return "", errors.New("Git 候选包含设备收件目录的变更")
	}
	return oid, nil
}

func (work Workspace) loadBase(ctx context.Context, ref string) (string, error) {
	path := filepath.Join(work.taskDir, "base.oid")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > 65 {
			return "", errors.New("Git 任务基线记录格式无效")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		oid := strings.TrimSuffix(string(data), "\n")
		return oid, work.verifyBase(ctx, oid)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Lstat(work.FixDir); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("Git 修复工作区缺少原有基线记录")
	}
	oid, err := git(ctx, work.root, "show-ref", "--hash", "--verify", "--", ref)
	if err != nil {
		return "", fmt.Errorf("无法读取 Git 基线引用: %w", err)
	}
	oid = strings.TrimSpace(oid)
	if err := work.verifyBase(ctx, oid); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(work.taskDir, "base-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(oid + "\n")
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return "", err
	}
	return oid, os.Rename(file.Name(), path)
}

func (work Workspace) verifyBase(ctx context.Context, oid string) error {
	if !validOID(oid) {
		return errors.New("Git 基线提交 ID 无效")
	}
	object, err := git(ctx, work.root, "cat-file", "-t", oid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(object) != "commit" {
		return errors.New("Git 基线须直接指向提交")
	}
	return nil
}

func (work Workspace) addWorktree(ctx context.Context, path, oid string) error {
	_, err := git(ctx, work.root, "-c", "core.hooksPath=/dev/null", "-c", "core.sparseCheckout=false",
		"worktree", "add", "--detach", "--", path, oid)
	return err
}

func (work Workspace) verifyWorktree(ctx context.Context, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !samePath(dir, resolved) || overlaps(work.root, resolved) {
		return errors.New("Git 任务工作区路径已变更")
	}
	gitFile, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !gitFile.Mode().IsRegular() {
		return errors.New("Git 任务工作区缺少有效的 Git 链接记录")
	}
	if err := verifyTopLevel(ctx, dir); err != nil {
		return err
	}
	common, err := gitCommonDirectory(ctx, dir)
	if err != nil {
		return err
	}
	if !samePath(work.common, common) {
		return errors.New("Git 任务工作区已切换到其他仓库")
	}
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	gitDir = trimLine(gitDir)
	if !samePath(filepath.Dir(gitDir), filepath.Join(common, "worktrees")) {
		return errors.New("Git 任务工作区缺少仓库工作区登记")
	}
	backlink, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return errors.New("无法读取 Git 工作区登记路径")
	}
	link := trimLine(string(backlink))
	if !filepath.IsAbs(link) {
		link = filepath.Join(gitDir, link)
	}
	if !samePath(link, filepath.Join(dir, ".git")) {
		return errors.New("Git 工作区登记已指向其他路径")
	}
	return nil
}

func verifyTopLevel(ctx context.Context, dir string) error {
	root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("绑定目录须为 Git 工作区根目录: %w", err)
	}
	if !samePath(dir, trimLine(root)) {
		return errors.New("绑定目录须为 Git 工作区根目录")
	}
	return nil
}

func gitCommonDirectory(ctx context.Context, dir string) (string, error) {
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(trimLine(common))
}

func worktreeHead(ctx context.Context, dir string) (string, error) {
	_, code, err := runGit(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	if code != 1 {
		return "", errors.New("Git 任务工作区须保持 detached HEAD")
	}
	oid, err := git(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	oid = strings.TrimSpace(oid)
	if !validOID(oid) {
		return "", errors.New("Git 候选提交 ID 无效")
	}
	return oid, nil
}

func requireClean(ctx context.Context, dir string, includeUntracked bool) error {
	mode := "no"
	if includeUntracked {
		mode = "all"
	}
	status, err := git(ctx, dir, "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--ignore-submodules=none",
		"--untracked-files="+mode)
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("Git 工作区中有尚未纳入候选提交的变更")
	}
	return nil
}

var oidPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func validOID(oid string) bool {
	return oidPattern.MatchString(oid)
}

func resolveTaskDirectory(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	// 缺少末级目录时先解析已存在的父目录，避免创建目录时跟随指向仓库的链接。
	parentPath := filepath.Dir(path)
	if parentPath == path {
		return "", err
	}
	parent, err := resolveTaskDirectory(parentPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func samePath(a, b string) bool {
	relative, err := filepath.Rel(a, b)
	return err == nil && relative == "."
}

func overlaps(a, b string) bool {
	within := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

func trimLine(value string) string {
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
}
