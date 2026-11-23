package runner

import (
	"context"
	"io"
)

// RunProjectCommand 在项目目录执行已由管理员选定的程序，并将两个输出流写入 output。
// 参数按原值传递，stdin 立即关闭。output 为 nil 时丢弃输出。
// 成功启动的进程以非零状态退出时返回该退出码，启动、输出和取消错误由 err 返回。
func RunProjectCommand(ctx context.Context, dir, program string, args []string, output io.Writer) (exitCode uint32, err error) {
	return RunProjectCommandStreams(ctx, dir, program, args, output, output)
}

// RunProjectCommandStreams 在设备上执行管理员选定的程序，分别写入 stdout 和 stderr。
// dir 使用绝对路径，program 使用绝对路径或 PATH 中的命令名。nil 输出流会被丢弃。
// 写入按次串行，两个输出流可共用一个 writer。返回前确认本轮进程树已退出，确认失败时返回错误。
func RunProjectCommandStreams(ctx context.Context, dir, program string, args []string, stdout, stderr io.Writer) (exitCode uint32, err error) {
	return runProjectCommand(ctx, dir, program, args, stdout, stderr)
}
