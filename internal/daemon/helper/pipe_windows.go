package helper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const pipePrefix = `\\.\pipe\agent-muster-`

func listen() (net.Listener, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	descriptor := "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GRGW;;;" + user.User.Sid.String() + ")"
	// Codex's elevated Windows sandbox may run child commands under a
	// different local Windows identity. The per-run token still gates requests.
	if sandbox, _, _, err := windows.LookupSID("", "CodexSandboxUsers"); err == nil {
		descriptor += "(A;;GRGW;;;" + sandbox.String() + ")"
	}
	return winio.ListenPipe(pipePrefix+hex.EncodeToString(random[:]), &winio.PipeConfig{SecurityDescriptor: descriptor})
}

func dial(ctx context.Context, address string) (net.Conn, error) {
	suffix, ok := strings.CutPrefix(address, pipePrefix)
	if !ok || len(suffix) != 32 {
		return nil, errors.New("无效的本机 Agent Muster 命名管道")
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		return nil, err
	}
	return winio.DialPipeContext(ctx, address)
}
