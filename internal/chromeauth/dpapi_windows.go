//go:build windows

package chromeauth

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func unprotectChromeDPAPI(encrypted []byte) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, fmt.Errorf("DPAPI 输入为空")
	}
	in := windows.DataBlob{Size: uint32(len(encrypted)), Data: &encrypted[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	runtime.KeepAlive(encrypted)
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	if out.Size != 32 || out.Data == nil {
		return nil, fmt.Errorf("DPAPI 返回密钥长度异常")
	}
	plaintext := unsafe.Slice(out.Data, int(out.Size))
	defer clear(plaintext)
	return append([]byte(nil), plaintext...), nil
}

func rejectChromeReparsePath(p string) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return fmt.Errorf("Chrome 来源路径无效")
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		return fmt.Errorf("读取 Chrome 来源路径属性失败: %w", err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("不支持 Chrome 来源 reparse point 或目录别名")
	}
	return nil
}
