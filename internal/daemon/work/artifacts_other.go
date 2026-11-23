//go:build !windows

package work

import (
	"io/fs"
	"os"
)

const artifactPlatformSupported = false

func openArtifactFile(string, string) (*os.File, error) {
	return nil, fs.ErrInvalid
}
