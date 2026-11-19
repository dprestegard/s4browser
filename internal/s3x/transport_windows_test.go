package s3x_test

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"

	"s4browser/internal/s3x"
)

// TestSendBufferReachesTheSocket reads the value back from Windows, which is
// where the 64 KiB default limited uploads.
func TestSendBufferReachesTheSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
		}
	}()

	transport := &http.Transport{}
	s3x.TuneTransport(transport)
	conn, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer func() { <-accepted }()

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		got, optErr = syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	}); err != nil {
		t.Fatal(err)
	}
	if optErr != nil {
		t.Fatal(optErr)
	}
	if got != s3x.SendBufferSize {
		t.Fatalf("SO_SNDBUF = %d, want %d", got, s3x.SendBufferSize)
	}
}
