package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

func runProjectCommand(ctx context.Context, dir, program string, args []string, stdout, stderr io.Writer) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !filepath.IsAbs(dir) {
		return 0, errors.New("项目命令需要绝对工作目录")
	}
	if program == "" || !filepath.IsAbs(program) && (filepath.Base(program) != program || filepath.VolumeName(program) != "") {
		return 0, errors.New("项目命令需要绝对程序路径或 PATH 中的命令名")
	}
	executable, err := exec.LookPath(program)
	if err != nil {
		return 0, fmt.Errorf("无法找到项目命令: %w", err)
	}
	cmd, err := probeCommand(ctx, executable, args...)
	if err != nil {
		return 0, err
	}
	if cmd.Err != nil {
		return 0, cmd.Err
	}
	cmd.Dir = dir
	cmd.Env = filteredAgentEnvironment(cmd.Environ(), nil)
	if name := strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe"); name == "git" {
		cmd.Env = boundGitEnvironment(cmd.Env)
	}
	var outputLock sync.Mutex
	serial := func(w io.Writer) io.Writer {
		if w == nil {
			return io.Discard
		}
		return projectOutput{writer: w, lock: &outputLock}
	}
	return runInJobWithStderr(ctx, cmd, "", func() error { return nil }, func(reader io.Reader) error {
		_, err := io.Copy(serial(stdout), reader)
		return err
	}, serial(stderr))
}

// boundGitEnvironment 保留正常用户配置及认证，移除可将 Git 操作重定向到其他仓库的继承变量。
func boundGitEnvironment(environment []string) []string {
	filtered := environment[:0]
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		key = strings.ToUpper(key)
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX", "GIT_CONFIG",
			"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_QUARANTINE_PATH", "GIT_SHALLOW_FILE",
			"GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE", "GIT_IMPLICIT_WORK_TREE":
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		filtered = append(filtered, item)
	}
	return append(filtered, "GIT_NO_REPLACE_OBJECTS=1")
}

type projectOutput struct {
	writer io.Writer
	lock   *sync.Mutex
}

// Write 序列化两个输出流的写入，以支持共用一个输出目标。
func (output projectOutput) Write(data []byte) (int, error) {
	output.lock.Lock()
	defer output.lock.Unlock()
	return output.writer.Write(data)
}
