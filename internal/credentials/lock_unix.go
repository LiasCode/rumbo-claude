//go:build unix

package credentials

import (
	"errors"
	"os"
	"syscall"
)

// tryLock locks file for this process alone, or says it is held by
// another, at once. Closing the file releases the lock.
func tryLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return false, nil
	}

	return err == nil, err
}
