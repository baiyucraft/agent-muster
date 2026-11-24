package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openProcessProofDirectory 限制退出凭据写入权限，并在执行循环存活期间固定目录位置。
func openProcessProofDirectory(root string) (*os.File, error) {
	parent, err := openLocalDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("核对工作目录: %w", err)
	}
	defer parent.Close()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// 子文件继承仅允许当前用户和 SYSTEM 的 ACL，覆盖父目录的继承设置。
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	const name = "process-proofs"
	path, err := windows.UTF16PtrFromString(filepath.Join(root, name))
	if err != nil {
		return nil, err
	}
	attrs := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	if err := windows.CreateDirectory(path, &attrs); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("创建受限目录: %w", err)
	}
	directory, err := openLocalChild(parent, name, true, windows.READ_CONTROL, localShareReadWrite)
	if err != nil {
		return nil, fmt.Errorf("打开受限目录: %w", err)
	}
	actual, err := windows.GetSecurityInfo(windows.Handle(directory.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		directory.Close()
		return nil, fmt.Errorf("读取目录访问权限: %w", err)
	}
	if actual.String() != descriptor.String() {
		directory.Close()
		return nil, errors.New("本机退出凭据目录访问权限与预期不符")
	}
	return directory, nil
}
