package s3x

import (
	"net/http"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"s4browser/internal/engine"
)

// Transport buffer sizes, chosen for multi-gigabit uploads.
const (
	transportBufferSize   = 64 * 1024
	expectContinueTimeout = time.Second
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
}

// NewHTTPClient returns the SDK HTTP client with the tuned transport. Stall
// detection belongs to the engine's watchdog, so no overall timeout is set.
func NewHTTPClient() *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().WithTransportOptions(TuneTransport)
}
