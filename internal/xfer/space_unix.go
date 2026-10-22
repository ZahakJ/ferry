package xfer

import "golang.org/x/sys/unix"

// freeSpace is how many bytes an unprivileged user can still write in dir.
func freeSpace(dir string) (int64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * int64(s.Bsize), nil
}
