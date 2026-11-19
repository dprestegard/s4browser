package s3x

import (
	"context"
	"net"
	"net/http"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"s4browser/internal/engine"
)

// Transport buffer sizes, chosen for multi-gigabit uploads.
const (
	transportBufferSize   = 64 * 1024
	expectContinueTimeout = time.Second

	// SendBufferSize is the socket send buffer of every S3 connection. Windows
	// gives a Go socket 64 KiB, which caps a connection at about 64 KiB per round
	// trip: 2.7 MiB/s at the 29 ms measured to S3 in stage 0.7. 4 MiB allows about
	// 140 MiB/s per connection at that latency.
	SendBufferSize = 4 * 1024 * 1024

	dialTimeout   = 30 * time.Second
	dialKeepAlive = 30 * time.Second
)

// TuneTransport applies the settings large transfers need. The SDK's default of
// 10 idle connections per host would force repeated TLS handshakes at higher
// concurrency. ExpectContinueTimeout must stay non-zero: zero sends the body
// without waiting for the server's answer.
func TuneTransport(t *http.Transport) {
	t.MaxConnsPerHost = 0
	t.MaxIdleConns = engine.MaxActivePartsLimit
	t.MaxIdleConnsPerHost = engine.MaxActivePartsLimit
	t.ExpectContinueTimeout = expectContinueTimeout
	t.ReadBufferSize = transportBufferSize
	t.WriteBufferSize = transportBufferSize
	t.DialContext = withSendBuffer(t.DialContext)
}

// withSendBuffer wraps dial so each new connection gets SendBufferSize. The
// buffer is only a performance setting, so a socket that refuses it still works.
func withSendBuffer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}).DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if buffered, ok := conn.(interface{ SetWriteBuffer(int) error }); ok {
			_ = buffered.SetWriteBuffer(SendBufferSize)
		}
		return conn, nil
	}
}

// NewHTTPClient returns the SDK HTTP client with the tuned transport. Stall
// detection belongs to the engine's watchdog, so no overall timeout is set.
func NewHTTPClient() *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().WithTransportOptions(TuneTransport)
}
