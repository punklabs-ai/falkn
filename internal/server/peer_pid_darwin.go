//go:build darwin

package server

import "golang.org/x/sys/unix"

func socketPeerPID(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
