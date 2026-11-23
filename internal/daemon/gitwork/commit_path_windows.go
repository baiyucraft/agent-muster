package gitwork

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// hasCommitReparsePoint 根据 Windows 原始文件属性识别重解析点，包括 Go 仍视为普通文件的类型。
func hasCommitReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
