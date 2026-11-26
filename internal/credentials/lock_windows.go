//go:build windows

package credentials

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx is called from kernel32 itself, which spares this module a
// dependency for one call.
var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
)

// tryLock locks file for this process alone, or says it is held by
// another, at once. Closing the file releases the lock. Windows' locks are
// mandatory, so the byte locked is one far past anything the file holds.
func tryLock(file *os.File) (bool, error) {
	overlapped := syscall.Overlapped{Offset: 0xFFFFFFFF, OffsetHigh: 0x7FFFFFFF}

	ok, _, err := lockFileEx.Call(
		file.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)

	if ok != 0 {
		return true, nil
	}

	if errors.Is(err, errorLockViolation) {
		return false, nil
	}

	return false, err
}
