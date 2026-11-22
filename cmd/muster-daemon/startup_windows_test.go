package main

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLoginStartupCommand(t *testing.T) {
	exe := "C:\\应用 & 工具\\Agent Muster\\muster-daemon.exe"
	command, err := loginStartupCommand(exe, "office-2")
	if err != nil {
		t.Fatal(err)
	}
	args, err := windows.DecomposeCommandLine(command)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{exe, "--background", "--profile=office-2"}
	if !slices.Equal(args, want) {
		t.Fatalf("启动参数为 %q，期望 %q", args, want)
	}
}

func TestLoginStartupCommandRejectsInvalidPath(t *testing.T) {
	for _, exe := range []string{"muster-daemon.exe", "C:\\app\"\\muster-daemon.exe", "C:\\" + strings.Repeat("桩", 260) + "\\muster-daemon.exe"} {
		if _, err := loginStartupCommand(exe, "default"); err == nil {
			t.Errorf("无效路径 %q 应被拒绝", exe)
		}
	}
}

func TestLoginStartupValueName(t *testing.T) {
	for _, profile := range []string{"default", "office-2"} {
		name, err := loginStartupValueName(profile)
		if err != nil || name != "AgentMuster-"+profile {
			t.Errorf("配置 %q 的注册表值名为 %q，错误 %v", profile, name, err)
		}
	}
	if _, err := loginStartupValueName("../office"); err == nil {
		t.Fatal("不合法的配置名应被拒绝")
	}
}
