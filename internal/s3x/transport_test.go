package s3x_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/fakes3"
	"s4browser/internal/s3x"
)

// sendBufferConn records the send buffer requested for a real connection.
type sendBufferConn struct {
	*net.TCPConn
	mu        sync.Mutex
	requested []int
}

func (c *sendBufferConn) SetWriteBuffer(n int) error {
	c.mu.Lock()
	c.requested = append(c.requested, n)
	c.mu.Unlock()
	return c.TCPConn.SetWriteBuffer(n)
}

// TestTunedTransportSetsSendBuffer sends a real request through the fake, so a
// transport that skipped the buffer, or broke dialing, fails here.
func TestTunedTransportSetsSendBuffer(t *testing.T) {
	server := newServer(t)
	base, ok := server.HTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("fake transport is %T", server.HTTPClient().Transport)
	}

	var mu sync.Mutex
	var conns []*sendBufferConn
	transport := base.Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		recorded := &sendBufferConn{TCPConn: conn.(*net.TCPConn)}
		mu.Lock()
		conns = append(conns, recorded)
		mu.Unlock()
		return recorded, nil
	}
	s3x.TuneTransport(transport)
	t.Cleanup(transport.CloseIdleConnections)

	client := server.S3Client(func(o *s3.Options) { o.HTTPClient = &http.Client{Transport: transport} })
	_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(fakes3.DefaultBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(make([]byte, 1024)),
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(conns) == 0 {
		t.Fatal("the tuned dialer was not used")
	}
	for i, conn := range conns {
		conn.mu.Lock()
		requested := append([]int(nil), conn.requested...)
		conn.mu.Unlock()
		if len(requested) != 1 || requested[0] != s3x.SendBufferSize {
			t.Fatalf("connection %d: send buffer requests %v, want one of %d", i, requested, s3x.SendBufferSize)
		}
	}
}

// TestTuneTransportDialsWithoutABaseDialer covers a transport with no dialer.
func TestTuneTransportDialsWithoutABaseDialer(t *testing.T) {
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
	conn.Close()
	<-accepted
}
