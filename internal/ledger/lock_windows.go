//go:build windows

package ledger

import (
	"os"
	"syscall"
	"unsafe"
)

// fileLock holds an OS-level exclusive lock on a single lock file for the
// duration of one atomic read-prev / compute-hash / append sequence.
// On Windows this uses LockFileEx over the whole file, blocking until
// acquired, released with UnlockFileEx.
type fileLock struct {
	f *os.File
}

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	ol := new(syscall.Overlapped)
	// Block (do not pass FAIL_IMMEDIATELY) until the exclusive lock is granted.
	r, _, err := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock),
		0,
		uintptr(0xFFFFFFFF),
		uintptr(0xFFFFFFFF),
		uintptr(unsafe.Pointer(ol)),
	)
	if r == 0 {
		f.Close()
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	ol := new(syscall.Overlapped)
	procUnlockFileEx.Call(
		l.f.Fd(),
		0,
		uintptr(0xFFFFFFFF),
		uintptr(0xFFFFFFFF),
		uintptr(unsafe.Pointer(ol)),
	)
	return l.f.Close()
}
