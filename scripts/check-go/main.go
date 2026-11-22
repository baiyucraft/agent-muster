// Package main 检查工作区的 Go 格式，并在当前平台验证根模块和桌面模块。
package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const desktopDir = "cmd/muster-daemon"

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check() error {
	if err := checkFormat(); err != nil {
		return err
	}
	if err := checkModule("."); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := buildDesktopAssets(); err != nil {
			return err
		}
		return checkModule(desktopDir, "-tags=production")
	}
	return checkModule(desktopDir)
}

func checkFormat() error {
	list := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--deduplicate", "-z", "--", "*.go")
	list.Stderr = os.Stderr
	output, err := list.Output()
	if err != nil {
		return fmt.Errorf("列出工作区 Go 源码: %w", err)
	}
	var findings []error
	for name := range bytes.SplitSeq(output, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		path := string(name)
		source, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			// 已从工作区删除的路径仍可能出现在 Git 索引里。
			continue
		}
		if err != nil {
			return err
		}
		// Git 在 Windows 检出时可能转换行尾，比较时只检查剩余的 Go 格式。
		source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
		formatted, err := format.Source(source)
		if err != nil {
			findings = append(findings, fmt.Errorf("%s: %w", path, err))
		} else if !bytes.Equal(source, formatted) {
			findings = append(findings, fmt.Errorf("%s 需要运行 gofmt", path))
		}
	}
	if err := errors.Join(findings...); err != nil {
		return err
	}
	fmt.Println("Go 格式检查通过")
	return nil
}

func checkModule(dir string, flags ...string) error {
	for _, action := range []string{"vet", "test"} {
		args := append([]string{action, "-mod=readonly"}, flags...)
		if err := run(dir, "go", append(args, "./...")...); err != nil {
			return err
		}
	}
	return nil
}

func buildDesktopAssets() error {
	pnpm := os.Getenv("npm_execpath")
	if pnpm == "" {
		return errors.New("请用 pnpm run check:go 运行 Windows 桌面检查")
	}
	args := []string{"--filter", "@agent-muster/device-ui", "run", "build"}
	// pnpm 安装方式不同，生命周期入口可能是脚本或原生可执行文件。
	switch strings.ToLower(filepath.Ext(pnpm)) {
	case ".js", ".cjs", ".mjs":
		pnpm, args = "node", append([]string{pnpm}, args...)
	}
	if _, err := exec.LookPath("wails3"); err != nil {
		return fmt.Errorf("Windows 桌面检查需要 PATH 中的 wails3: %w", err)
	}
	if err := run(".", pnpm, args...); err != nil {
		return err
	}
	return run(desktopDir, "wails3", "generate", "icons", "-input", "build/appicon.png", "-windowsfilename", "build/windows/icon.ico", "-macfilename", "")
}

func run(dir, name string, args ...string) error {
	fmt.Printf("[%s] %s %s\n", dir, filepath.Base(name), strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("[%s] %s 检查失败: %w", dir, filepath.Base(name), err)
	}
	return nil
}
