package gitwork

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrPushRejected 表示 Git 收到了目标分支的明确拒绝回执。
var ErrPushRejected = errors.New("Git 远端拒绝更新目标分支")

// PushDestination 保存仅在设备使用的实际推送端点。持久化或上报时使用 Fingerprint。
type PushDestination struct {
	endpoint string
	protocol string
}

// Fingerprint 返回推送端点的 SHA-256 标识。空端点返回空字符串。
func (destination PushDestination) Fingerprint() string {
	if destination.endpoint == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(destination.endpoint))
	return hex.EncodeToString(sum[:])
}

var pushRemoteName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var sshEndpoint = regexp.MustCompile(`^(?:[A-Za-z0-9_.-]+@)?(?:[A-Za-z0-9_][A-Za-z0-9_.-]*|\[[0-9a-fA-F:]+\]):[^:].*$`)

// ResolvePushDestination 解析远端的唯一有效推送地址，包括 pushurl 和 Git 的 URL 重写。
// 本地相对地址以传入目录为准。此操作只读取本机配置。
func ResolvePushDestination(ctx context.Context, dir, remote string) (PushDestination, error) {
	if !pushRemoteName.MatchString(remote) {
		return PushDestination{}, errors.New("Git 推送远端须为已配置的别名")
	}
	output, err := git(ctx, dir, "remote", "get-url", "--push", "--all", "--", remote)
	if err != nil {
		return PushDestination{}, fmt.Errorf("无法读取 Git 推送远端: %w", err)
	}
	endpoint := trimLine(output)
	if endpoint == "" || strings.ContainsAny(endpoint, "\r\n\x00") {
		return PushDestination{}, errors.New("Git 远端必须只配置一个推送地址")
	}
	return parseDestination(dir, endpoint)
}

func parseDestination(dir, endpoint string) (PushDestination, error) {
	if strings.Contains(endpoint, "::") && !strings.Contains(endpoint, "://") && !sshEndpoint.MatchString(endpoint) {
		return PushDestination{}, errors.New("Git 推送地址使用了不受支持的传输协议")
	}
	if strings.Contains(endpoint, "://") {
		address, err := url.Parse(endpoint)
		if err != nil {
			return PushDestination{}, errors.New("Git 推送地址格式无效")
		}
		switch address.Scheme {
		case "https", "http", "ssh":
			if address.Hostname() != "" {
				return PushDestination{endpoint, address.Scheme}, nil
			}
		case "file":
			if (address.Host == "" || address.Host == "localhost") && address.User == nil && address.Path != "" && !strings.HasPrefix(strings.ReplaceAll(address.Path, `\`, "/"), "//") {
				return PushDestination{endpoint, "file"}, nil
			}
		}
		return PushDestination{}, errors.New("Git 推送地址使用了不受支持的传输协议")
	}
	if !filepath.IsAbs(endpoint) && sshEndpoint.MatchString(endpoint) {
		return PushDestination{endpoint, "ssh"}, nil
	}
	if !filepath.IsAbs(endpoint) {
		if strings.Contains(endpoint, ":") || strings.HasPrefix(strings.ReplaceAll(endpoint, `\`, "/"), "/") {
			return PushDestination{}, errors.New("Git 本地推送路径格式无效")
		}
		endpoint = filepath.Join(dir, endpoint)
	}
	if !filepath.IsAbs(endpoint) || strings.HasPrefix(filepath.ToSlash(endpoint), "//") {
		return PushDestination{}, errors.New("Git 本地推送地址必须为本机路径")
	}
	return PushDestination{filepath.Clean(endpoint), "file"}, nil
}

// PushCandidate 在 beforePush 核准后推送候选至指定分支，保留 Git 钩子与认证。
// 调用方在 beforePush 内记录写入意图，并在返回之后使用 ConfirmCandidate 读回。
func PushCandidate(ctx context.Context, dir string, destination PushDestination, branch, candidate string, beforePush func() error) error {
	if err := verifyPublication(ctx, dir, destination, branch, candidate); err != nil {
		return err
	}
	return withPinnedRemote(destination, func(config []string, remote string) error {
		if err := beforePush(); err != nil {
			return err
		}
		// 修订后缀使源端按提交 ID 解析，即使本地有同名分支。
		output, code, err := runGit(ctx, dir, append(config, "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", remote, candidate+"^{commit}:"+branch)...)
		if err != nil || code == 0 {
			return err
		}
		for line := range strings.SplitSeq(output, "\n") {
			fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
			if len(fields) == 3 && fields[0] == "!" && fields[1] == candidate+"^{commit}:"+branch &&
				(strings.HasPrefix(fields[2], "[rejected]") || strings.HasPrefix(fields[2], "[remote rejected]")) {
				return ErrPushRejected
			}
		}
		return fmt.Errorf("Git 推送未获得明确回执（退出码 %d）", code)
	})
}

// ConfirmCandidate 读取实际推送地址上的指定分支。分支不存在时返回空 head 和 false。
// 远端分支指向后续提交时按需抓取提交对象，并验证候选是已读回分支头的祖先。
func ConfirmCandidate(ctx context.Context, dir string, destination PushDestination, branch, candidate string) (head string, contains bool, err error) {
	if err = verifyPublication(ctx, dir, destination, branch, candidate); err != nil {
		return "", false, err
	}
	err = withPinnedRemote(destination, func(config []string, remote string) error {
		output, code, err := runGit(ctx, dir, append(config, "ls-remote", "--exit-code", "--refs", "--", remote, branch)...)
		if err != nil {
			return err
		}
		if code == 2 && output == "" {
			return nil
		}
		if code != 0 {
			return fmt.Errorf("无法读取 Git 远端分支（退出码 %d）", code)
		}
		oid, ref, found := strings.Cut(trimLine(output), "\t")
		if !found || ref != branch || !validOID(oid) || len(oid) != len(candidate) {
			return errors.New("Git 远端返回了不匹配的分支引用")
		}
		head = oid
		if head == candidate {
			contains = true
			return nil
		}
		if _, code, err := runGit(ctx, dir, "cat-file", "-e", head+"^{commit}"); err != nil {
			return err
		} else if code != 0 {
			// 仅获取对象，保留本机关联分支和 FETCH_HEAD。
			if _, err := git(ctx, dir, append(config, "fetch", "--no-tags", "--no-prune", "--no-write-fetch-head", "--no-auto-maintenance", "--no-recurse-submodules", "--refmap=", "--", remote, branch)...); err != nil {
				return err
			}
		}
		_, code, err = runGit(ctx, dir, "merge-base", "--is-ancestor", candidate, head)
		if err != nil {
			return err
		}
		switch code {
		case 0:
			contains = true
		case 1:
		default:
			return fmt.Errorf("无法验证 Git 远端提交历史（退出码 %d）", code)
		}
		return nil
	})
	return head, contains, err
}

func verifyPublication(ctx context.Context, dir string, destination PushDestination, branch, candidate string) error {
	if destination.endpoint == "" || !validOID(candidate) || !strings.HasPrefix(branch, "refs/heads/") {
		return errors.New("Git 推送需要有效端点、提交 ID 及完整分支引用")
	}
	if _, err := git(ctx, dir, "check-ref-format", branch); err != nil {
		return fmt.Errorf("无法核对 Git 推送分支引用: %w", err)
	}
	object, err := git(ctx, dir, "cat-file", "-t", candidate)
	if err != nil {
		return fmt.Errorf("无法核对 Git 推送候选提交: %w", err)
	}
	if trimLine(object) != "commit" {
		return errors.New("Git 推送候选须直接指向当前仓库的提交")
	}
	return nil
}

func withPinnedRemote(destination PushDestination, invoke func(config []string, remote string) error) error {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	name := "muster-" + hex.EncodeToString(entropy[:])
	marker := name + ":destination"
	// Git 的 URL 重写不递归。只把随机地址重写为已解析端点，避免同一规则再次改写端点。
	file, err := os.CreateTemp("", "muster-git-config-*")
	if err != nil {
		return errors.New("无法创建 Git 推送配置")
	}
	defer os.Remove(file.Name())
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(destination.endpoint)
	_, writeErr := fmt.Fprintf(file, "[remote \"%s\"]\n\turl = %s\n\tpushurl = %s\n[url \"%s\"]\n\tinsteadOf = %s\n", name, marker, marker, quoted, marker)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return errors.New("无法写入 Git 推送配置")
	}
	return invoke([]string{"-c", "include.path=" + filepath.ToSlash(file.Name()), "-c", "protocol.allow=never", "-c", "protocol." + destination.protocol + ".allow=always", "-c", "http.followRedirects=false"}, name)
}
