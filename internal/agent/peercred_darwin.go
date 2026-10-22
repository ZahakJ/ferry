package agent

import "golang.org/x/sys/unix"

// peerUID is the user ID of the process on the other end of a unix socket.
func peerUID(fd int) (int, error) {
	c, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(c.Uid), nil
}
