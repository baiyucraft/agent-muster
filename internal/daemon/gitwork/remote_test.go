package gitwork

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const publicationBranch = "refs/heads/muster/fix"

func allowFixturePush() error { return nil }

func TestPublicationUsesEffectivePushAddressAndConfirmsAncestry(t *testing.T) {
	repo, first := publicationRepo(t)
	push := t.TempDir()
	pull := t.TempDir()
	wrong := t.TempDir()
	for _, path := range []string{push, pull, wrong} {
		publicationGit(t, path, "init", "--bare")
	}
	publicationGit(t, repo, "remote", "add", "origin", pull)
	publicationGit(t, repo, "remote", "set-url", "--push", "origin", "muster-fixture:push.git")
	publicationGit(t, repo, "config", "url."+push+".insteadOf", "muster-fixture:push.git")
	publicationGit(t, repo, "config", "url."+wrong+".insteadOf", push)
	publicationGit(t, repo, "commit", "--allow-empty", "-m", "different commit")
	publicationGit(t, repo, "branch", first, "HEAD")

	destination, err := ResolvePushDestination(t.Context(), repo, "origin")
	if err != nil || len(destination.Fingerprint()) != 64 {
		t.Fatalf("远端指纹为 %q，错误为 %v", destination.Fingerprint(), err)
	}
	if data, err := json.Marshal(destination); err != nil || string(data) != "{}" {
		t.Fatalf("远端地址须仅在设备内使用，序列化返回 %q 和 %v", data, err)
	}
	assertPublication(t, repo, destination, first, "", false)
	if err := PushCandidate(t.Context(), repo, destination, publicationBranch, first, allowFixturePush); err != nil {
		t.Fatal(err)
	}
	assertPublication(t, repo, destination, first, first, true)
	if got := publicationGit(t, push, "rev-parse", publicationBranch); got != first {
		t.Fatalf("推送仓库分支为 %s，期望 %s", got, first)
	}
	for _, path := range []string{pull, wrong} {
		if got := publicationGit(t, path, "for-each-ref", publicationBranch); got != "" {
			t.Fatal("推送修改了另一个仓库")
		}
	}

	other, _ := publicationRepo(t)
	publicationGit(t, other, "fetch", push, publicationBranch)
	publicationGit(t, other, "checkout", "--detach", "FETCH_HEAD")
	publicationGit(t, other, "commit", "--allow-empty", "-m", "next")
	next := publicationGit(t, other, "rev-parse", "HEAD")
	publicationGit(t, other, "push", push, "HEAD:"+publicationBranch)
	assertPublication(t, repo, destination, first, next, true)
	if err := PushCandidate(t.Context(), repo, destination, publicationBranch, first, allowFixturePush); err == nil {
		t.Fatal("远端接受了非快进推送")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("读回不应写入 FETCH_HEAD，Stat 返回 %v", err)
	}
	publicationGit(t, repo, "remote", "set-url", "--push", "origin", pull)
	updated, err := ResolvePushDestination(t.Context(), repo, "origin")
	if err != nil || destination.Fingerprint() == updated.Fingerprint() {
		t.Fatalf("推送端点变更后应产生新指纹，错误为 %v", err)
	}
}

func TestPublicationHonorsPrePushHookAndRejectsMultipleAddresses(t *testing.T) {
	repo, candidate := publicationRepo(t)
	push := t.TempDir()
	publicationGit(t, push, "init", "--bare")
	publicationGit(t, repo, "remote", "add", "origin", push)
	destination, err := ResolvePushDestination(t.Context(), repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	publicationGit(t, repo, "config", "core.hooksPath", filepath.ToSlash(hooks))
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PushCandidate(t.Context(), repo, destination, publicationBranch, candidate, allowFixturePush); err == nil {
		t.Fatal("没有执行拒绝推送的 pre-push 钩子")
	}
	assertPublication(t, repo, destination, candidate, "", false)
	publicationGit(t, repo, "remote", "set-url", "--add", "--push", "origin", push)
	publicationGit(t, repo, "remote", "set-url", "--add", "--push", "origin", t.TempDir())
	if _, err := ResolvePushDestination(t.Context(), repo, "origin"); err == nil {
		t.Fatal("接受了多个推送地址")
	}
}

func TestPublicationRejectsUnsupportedTransports(t *testing.T) {
	for _, address := range []string{"ext::git receive-pack", "custom::https://example.invalid/p", "git://example.invalid/p", "file://example.invalid/p", "file:////example.invalid/p", "-oProxyCommand=invalid:project", `\ambiguous-windows-path`} {
		if _, err := parseDestination(t.TempDir(), address); err == nil {
			t.Errorf("接受了地址 %q", address)
		}
	}
}

func publicationRepo(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("设备命令在 Windows 上执行")
	}
	if _, err := exec.LookPath("git.exe"); err != nil {
		t.Skip("本机缺少 Git CLI")
	}
	dir := t.TempDir()
	publicationGit(t, dir, "init")
	publicationGit(t, dir, "config", "user.email", "muster@example.invalid")
	publicationGit(t, dir, "config", "user.name", "Muster Fixture")
	publicationGit(t, dir, "commit", "--allow-empty", "-m", "fixture")
	return dir, publicationGit(t, dir, "rev-parse", "HEAD")
}

func publicationGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("本地 Git %v 失败：%v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func assertPublication(t *testing.T, repo string, destination PushDestination, candidate, wantHead string, wantContains bool) {
	t.Helper()
	head, contains, err := ConfirmCandidate(t.Context(), repo, destination, publicationBranch, candidate)
	if err != nil || head != wantHead || contains != wantContains {
		t.Fatalf("确认推送 head=%q contains=%v err=%v，期望 head=%q contains=%v", head, contains, err, wantHead, wantContains)
	}
}
