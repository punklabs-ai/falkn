package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonPIDIdentifiesUnixSocketPeer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var daemon net.Conn
	select {
	case daemon = <-accepted:
		defer daemon.Close()
	case err := <-acceptErrors:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("daemon did not accept the Unix socket connection")
	}

	pid, err := daemonPID(client)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("daemon PID = %d, want %d", pid, os.Getpid())
	}
}
