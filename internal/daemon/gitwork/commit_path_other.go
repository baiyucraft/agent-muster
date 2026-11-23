//go:build !windows

package gitwork

import "os"

func hasCommitReparsePoint(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
