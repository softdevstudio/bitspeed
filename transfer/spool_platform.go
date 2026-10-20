//go:build linux || darwin

package transfer

import (
	"math"
	"os"
	"syscall"
)

func spoolSupported() bool       { return true }
func lockSpool(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
func freeSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	blocks, size := uint64(stat.Bavail), uint64(stat.Bsize)
	if size == 0 {
		return 0, ErrStorage
	}
	if blocks > math.MaxInt64/size {
		return math.MaxInt64, nil
	}
	return int64(blocks * size), nil
}

// Nonblocking open prevents a raced-in FIFO from hanging preparation. Fstat
// still has to establish a regular file and the observed source identity.
func openSource(path string) (*os.File, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), path), nil
}
