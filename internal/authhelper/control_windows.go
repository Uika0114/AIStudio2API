package authhelper

import (
	"context"
	"os"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func stopEventName(pid int) string { return "Local\\AIStudio2APIAuthHelperStop-" + strconv.Itoa(pid) }

// This local event is restricted to the original user's SID. It does not expose a
// remote control endpoint, carry credentials or terminate an arbitrary process.
func localStopContext(parent context.Context) (context.Context, func(), error) {
	sid, err := WindowsIdentity()
	if err != nil {
		return nil, nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, nil, failure("STATE_UNAVAILABLE")
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(stopEventName(os.Getpid()))
	if err != nil {
		return nil, nil, failure("STATE_UNAVAILABLE")
	}
	handle, err := windows.CreateEvent(&attrs, 1, 0, name)
	if err != nil {
		return nil, nil, failure("STATE_UNAVAILABLE")
	}
	ctx, cancel := context.WithCancel(parent)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		defer windows.CloseHandle(handle)
		for ctx.Err() == nil {
			status, e := windows.WaitForSingleObject(handle, 250)
			if e != nil || status == windows.WAIT_OBJECT_0 {
				cancel()
				return
			}
		}
	}()
	return ctx, func() { cancel(); group.Wait() }, nil
}

func signalLocalStop(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return failure("INVALID_REQUEST")
	}
	name, err := windows.UTF16PtrFromString(stopEventName(pid))
	if err != nil {
		return failure("INVALID_REQUEST")
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	defer windows.CloseHandle(handle)
	if windows.SetEvent(handle) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return nil
}
