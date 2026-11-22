package credentials

import (
	"errors"
	"fmt"
	"os/user"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	maxCredentialBlob       = 2560
)

var (
	advapi       = syscall.NewLazyDLL("advapi32.dll")
	readCred     = advapi.NewProc("CredReadW")
	writeCred    = advapi.NewProc("CredWriteW")
	freeCred     = advapi.NewProc("CredFree")
	kernel       = syscall.NewLazyDLL("kernel32.dll")
	createMutex  = kernel.NewProc("CreateMutexW")
	releaseMutex = kernel.NewProc("ReleaseMutex")
)

// credential matches the native CREDENTIALW layout.
type credential struct {
	Flags          uint32
	Type           uint32
	TargetName     *uint16
	Comment        *uint16
	LastWritten    syscall.Filetime
	BlobSize       uint32
	Blob           *byte
	Persist        uint32
	AttributeCount uint32
	Attributes     unsafe.Pointer
	TargetAlias    *uint16
	UserName       *uint16
}

// Load 从当前用户的凭据管理器读取配对身份，缺少凭据时返回 ErrNotEnrolled。
func (store Store) Load() ([]byte, error) {
	target, err := syscall.UTF16PtrFromString(store.target)
	if err != nil {
		return nil, err
	}
	var stored *credential
	ok, _, callErr := readCred.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&stored)))
	if ok == 0 {
		if errors.Is(callErr, syscall.ERROR_NOT_FOUND) {
			return nil, ErrNotEnrolled
		}
		return nil, fmt.Errorf("读取 Windows 凭据管理器: %w", callErr)
	}
	defer freeCred.Call(uintptr(unsafe.Pointer(stored)))
	if stored == nil || stored.Blob == nil || stored.BlobSize == 0 || stored.BlobSize > maxCredentialBlob {
		return nil, errors.New("已保存的设备凭据格式无效")
	}
	return append([]byte(nil), unsafe.Slice(stored.Blob, stored.BlobSize)...), nil
}

// Save 校验凭据长度，并将设备身份写入当前用户的凭据管理器。
func (store Store) Save(data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBlob {
		return errors.New("设备凭据长度无效")
	}
	target, err := syscall.UTF16PtrFromString(store.target)
	if err != nil {
		return err
	}
	entry := credential{
		Type:       credTypeGeneric,
		TargetName: target,
		BlobSize:   uint32(len(data)),
		Blob:       &data[0],
		Persist:    credPersistLocalMachine,
	}
	ok, _, callErr := writeCred.Call(uintptr(unsafe.Pointer(&entry)), 0)
	runtime.KeepAlive(data)
	if ok == 0 {
		return fmt.Errorf("写入 Windows 凭据管理器: %w", callErr)
	}
	return nil
}

// DuringEnrollment 获取当前 Windows 会话内的用户和配置专属互斥锁。锁已占用时立即返回错误。
func (store Store) DuringEnrollment(enroll func() error) error {
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("获取当前 Windows 用户: %w", err)
	}
	name, err := syscall.UTF16PtrFromString("Local\\AgentMuster-enroll-" + currentUser.Uid + "-" + store.target)
	if err != nil {
		return err
	}

	mutex, _, callErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if mutex == 0 {
		return fmt.Errorf("建立设备配对互斥锁: %w", callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(mutex))

	// Windows mutex ownership belongs to an OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	event, err := syscall.WaitForSingleObject(syscall.Handle(mutex), 0)
	if err != nil {
		return fmt.Errorf("获取设备配对互斥锁: %w", err)
	}
	if event != syscall.WAIT_OBJECT_0 && event != syscall.WAIT_ABANDONED {
		return errors.New("当前配置正在配对")
	}
	defer releaseMutex.Call(mutex)
	return enroll()
}
