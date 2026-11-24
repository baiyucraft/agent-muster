//go:build !windows

package work

import "os"

// openProcessProofDirectory 在没有 Windows Job Object 的平台上保持现有执行策略。
func openProcessProofDirectory(string) (*os.File, error) { return nil, nil }
