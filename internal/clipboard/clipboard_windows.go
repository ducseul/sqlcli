package clipboard

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	openClipboard    = user32.NewProc("OpenClipboard")
	closeClipboard   = user32.NewProc("CloseClipboard")
	emptyClipboard   = user32.NewProc("EmptyClipboard")
	getClipboardData = user32.NewProc("GetClipboardData")
	setClipboardData = user32.NewProc("SetClipboardData")

	globalAlloc  = kernel32.NewProc("GlobalAlloc")
	globalFree   = kernel32.NewProc("GlobalFree")
	globalLock   = kernel32.NewProc("GlobalLock")
	globalUnlock = kernel32.NewProc("GlobalUnlock")
	moveMemory   = kernel32.NewProc("RtlMoveMemory")
	lstrlenW     = kernel32.NewProc("lstrlenW")
)

// open retries briefly: another process may hold the clipboard.
func open() error {
	for i := 0; i < 20; i++ {
		if r, _, _ := openClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("open clipboard: %w", ErrUnavailable)
}

func write(text string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := open(); err != nil {
		return err
	}
	defer closeClipboard.Call()

	if r, _, err := emptyClipboard.Call(); r == 0 {
		return fmt.Errorf("empty clipboard: %v", err)
	}
	data, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	size := uintptr(len(data) * 2)
	h, _, err := globalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("alloc: %v", err)
	}
	p, _, err := globalLock.Call(h)
	if p == 0 {
		globalFree.Call(h)
		return fmt.Errorf("lock: %v", err)
	}
	moveMemory.Call(p, uintptr(unsafe.Pointer(&data[0])), size)
	globalUnlock.Call(h)
	if r, _, err := setClipboardData.Call(cfUnicodeText, h); r == 0 {
		globalFree.Call(h)
		return fmt.Errorf("set clipboard: %v", err)
	}
	return nil // the system owns h now
}

func read() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := open(); err != nil {
		return "", err
	}
	defer closeClipboard.Call()

	h, _, _ := getClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil // empty or no text on the clipboard
	}
	p, _, err := globalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("lock: %v", err)
	}
	defer globalUnlock.Call(h)
	n, _, _ := lstrlenW.Call(p)
	if n == 0 {
		return "", nil
	}
	buf := make([]uint16, n)
	moveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, n*2)
	return syscall.UTF16ToString(buf), nil
}
