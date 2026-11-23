package work

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/baiyucraft/agent-muster/internal/runreport"
	"golang.org/x/sys/windows"
)

const (
	runLogPlatformSupported = true
	maxLogDirectories       = 256
)

func readLocalRunLogs(root, runID, invocation string) (runreport.LogResponse, error) {
	if !logRunID.MatchString(runID) || invocation != "" && !runreport.ValidReviewLogInvocation(invocation) {
		return runreport.LogResponse{}, fs.ErrInvalid
	}
	directory, err := openRunLogDirectory(filepath.Join(root, "logs", runID))
	if err != nil {
		return runreport.LogResponse{}, err
	}
	defer directory.Close()
	if invocation != "" {
		files, err := readLogInvocation(directory, invocation, runreport.MaxLogFiles)
		return runLogResponse(files), err
	}
	entries, err := directory.Readdir(maxLogDirectories + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return runreport.LogResponse{}, err
	}
	if len(entries) > maxLogDirectories {
		return runreport.LogResponse{}, errors.New("日志目录条目超过读取上限")
	}
	slices.SortFunc(entries, func(a, b os.FileInfo) int {
		if order := b.ModTime().Compare(a.ModTime()); order != 0 {
			return order
		}
		return cmp.Compare(a.Name(), b.Name())
	})
	var files []runreport.LogFile
	for _, entry := range entries {
		if len(files) == runreport.MaxLogFiles {
			break
		}
		if !runreport.IsLogInvocation(entry.Name()) || runreport.ValidReviewLogInvocation(entry.Name()) {
			continue
		}
		streams, err := readLogInvocation(directory, entry.Name(), runreport.MaxLogFiles-len(files))
		if err != nil {
			return runreport.LogResponse{}, err
		}
		files = append(files, streams...)
	}
	return runLogResponse(files), nil
}

func runLogResponse(files []runreport.LogFile) runreport.LogResponse {
	if len(files) == 0 {
		return runreport.LogResponse{Status: runreport.LogMissing}
	}
	return runreport.LogResponse{Status: runreport.LogAvailable, Files: files}
}

func readLogInvocation(directory *os.File, name string, remaining int) ([]runreport.LogFile, error) {
	invocation, err := openLogChild(directory, name, true, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer invocation.Close()
	files := make([]runreport.LogFile, 0, 2)
	for _, stream := range []string{"stdout", "stderr"} {
		if len(files) == remaining {
			break
		}
		file, err := openLogChild(invocation, stream+".log", false, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry, readErr := readLogTail(file, name, stream)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		files = append(files, entry)
	}
	return files, nil
}

func readLogTail(file *os.File, invocation, stream string) (runreport.LogFile, error) {
	info, err := file.Stat()
	if err != nil {
		return runreport.LogFile{}, err
	}
	const limit = runreport.MaxLogTextBytes / runreport.MaxLogFiles
	offset := max(0, info.Size()-limit)
	data := make([]byte, info.Size()-offset)
	n, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return runreport.LogFile{}, err
	}
	text := strings.ToValidUTF8(string(data[:n]), "�")
	truncated := offset != 0 || n != len(data) || len(text) > limit
	if len(text) > limit {
		start := len(text) - limit
		for !utf8.RuneStart(text[start]) {
			start++
		}
		text = text[start:]
	}
	updated := info.ModTime().UTC()
	return runreport.LogFile{
		Invocation: invocation, Stream: stream, Text: text,
		Truncated: truncated, UpdatedAt: &updated,
	}, nil
}

func openRunLogDirectory(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, fs.ErrInvalid
	}
	root := volume + `\`
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(handle), root)
	for part := range strings.SplitSeq(absolute[len(root):], `\`) {
		next, err := openLogChild(directory, part, true, 0)
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
		directory = next
	}
	return directory, nil
}

func openLogChild(parent *os.File, name string, directory bool, extraAccess uint32) (*os.File, error) {
	if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." || strings.ContainsAny(name, `:/\`) {
		return nil, fs.ErrInvalid
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()), ObjectName: objectName,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	options := uint32(windows.FILE_NON_DIRECTORY_FILE)
	access := uint32(windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE) | extraAccess
	if directory {
		options = windows.FILE_DIRECTORY_FILE
		access |= windows.FILE_TRAVERSE
	}
	// 按父目录句柄打开单一名称，重解析点无法改变读取边界。
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, &attributes, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !directory && info.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return nil, errors.Join(fs.ErrInvalid, err)
	}
	return os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name)), nil
}
