package runner

import "context"

type processProofKey struct{}

// WithProcessProof 让运行器先预留退出证据引用，待辅助进程确认就绪后调用 arm 持久化执行许可。
// reserve 返回设备受限目录内未使用的绝对路径，文件名为 exit-<随机小写十六进制>.proof。
func WithProcessProof(ctx context.Context, reserve func() (path string, arm func() error, err error)) context.Context {
	return context.WithValue(ctx, processProofKey{}, reserve)
}
