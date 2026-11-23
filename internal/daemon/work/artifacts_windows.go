package work

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const artifactPlatformSupported = true

func openArtifactFile(directory, path string) (*os.File, error) {
	parent, err := openLocalDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := openLocalChild(parent, part, true, 0, localShareReadWrite)
		if err != nil {
			return nil, err
		}
		defer next.Close()
		parent = next
	}
	// 仅共享读取。Windows 在已有写入句柄或可写映射时会拒绝本次打开。
	return openLocalChild(parent, parts[len(parts)-1], false, 0, windows.FILE_SHARE_READ)
}
