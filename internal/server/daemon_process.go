package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// StopDaemonProcess terminates the process serving the current user's Falkn
// socket. It exists for the one-time upgrade of daemons that predate the
// daemon.shutdown RPC and must only be called after confirming no sessions run.
func StopDaemonProcess() (int, error) {
	paths, err := RuntimePaths()
	if err != nil {
		return 0, err
	}
	connection, err := connect(paths.Socket)
	if err != nil {
		return 0, fmt.Errorf("connect to daemon socket: %w", err)
	}
	defer connection.Close()

	pid, err := daemonPID(connection)
	if err != nil {
		return 0, err
	}
	if pid <= 1 {
		return 0, fmt.Errorf("refusing to signal invalid daemon process %d", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("find daemon process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return 0, fmt.Errorf("stop daemon process %d: %w", pid, err)
	}
	return pid, nil
}

func daemonPID(connection net.Conn) (int, error) {
	rawConnection, ok := connection.(syscall.Conn)
	if !ok {
		return 0, errors.New("daemon connection does not expose its peer process")
	}
	raw, err := rawConnection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access daemon socket: %w", err)
	}
	var pid int
	var peerError error
	if err := raw.Control(func(fd uintptr) {
		pid, peerError = socketPeerPID(int(fd))
	}); err != nil {
		return 0, fmt.Errorf("inspect daemon socket: %w", err)
	}
	if peerError != nil {
		return 0, fmt.Errorf("identify daemon process: %w", peerError)
	}
	return pid, nil
}
