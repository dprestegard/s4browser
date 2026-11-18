//go:build !race

// Memory measurements are excluded under the race detector, which multiplies heap
// use. The S3 endpoint runs in a child process, so that only the client's heap is
// measured.

package s3x_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

const (
	endpointEnv = "S4BROWSER_MEASURE_ENDPOINT"
	memBaseEnv  = "S4BROWSER_MEASURE_MEMBASE"
)

func TestMain(m *testing.M) {
	if os.Getenv(endpointEnv) == "1" {
		runMeasureEndpoint()
		return
	}
	os.Exit(m.Run())
}

// runMeasureEndpoint is a minimal TLS endpoint that discards bodies.
func runMeasureEndpoint() {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"00000000000000000000000000000000"`)
		w.Header().Set("x-amz-checksum-crc32", "AAAAAA==")
		w.WriteHeader(http.StatusOK)
	}))
	server.StartTLS()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	fmt.Printf("%s %s\n", server.URL, base64.StdEncoding.EncodeToString(cert))
	_, _ = io.Copy(io.Discard, os.Stdin) // Runs until the parent closes stdin.
	server.CloseClientConnections()
}

// startEndpoint starts the child process and returns a tuned SDK client for it.
func startEndpoint(t *testing.T) *s3.Client {
	t.Helper()
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), endpointEnv+"=1")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	url, encoded, ok := strings.Cut(strings.TrimSpace(line), " ")
	certPEM, decodeErr := base64.StdEncoding.DecodeString(encoded)
	if !ok || decodeErr != nil {
		t.Fatalf("endpoint said %q", line)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	httpClient := s3x.NewHTTPClient().WithTransportOptions(func(transport *http.Transport) {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	})
	t.Cleanup(func() { httpClient.GetTransport().CloseIdleConnections() })
	return s3.New(s3.Options{
		Region: fakes3.Region, BaseEndpoint: aws.String(url), UsePathStyle: true, HTTPClient: httpClient,
		Credentials: credentials.NewStaticCredentialsProvider("TESTKEYID", "test-not-a-secret", ""),
	})
}

func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return int64(sample[0].Value.Uint64())
}

// heldBody serves part of a body, then blocks, so that the request stays in
// flight with all of its connection buffers allocated.
type heldBody struct {
	length, sent int64
	holdAt       int64
	reached      *atomic.Int64
	release      <-chan struct{}
	held         bool
}

func (b *heldBody) Read(p []byte) (int, error) {
	if b.sent >= b.holdAt && !b.held {
		b.held = true
		b.reached.Add(1)
		<-b.release
	}
	if b.sent >= b.length {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), b.length-b.sent))
	clear(p[:n])
	b.sent += int64(n)
	return n, nil
}

func (b *heldBody) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.sent = offset
	case io.SeekEnd:
		b.sent = b.length + offset
	default:
		b.sent += offset
	}
	return b.sent, nil
}

// measurePerConnection returns the client heap used by one in-flight upload.
func measurePerConnection(t *testing.T, client *s3.Client, connections int) int64 {
	t.Helper()
	store := s3x.NewStore(s3x.StaticClient(client))
	ctx := context.Background()
	warmRef := engine.ObjectRef{Region: fakes3.Region, Bucket: "bucket", Key: "warm"}
	// One finished request, so that client-wide lazy state is not charged per connection.
	if _, err := store.PutObject(ctx, engine.PutObjectRequest{Ref: warmRef, Body: strings.NewReader("warm"), ContentLength: 4}); err != nil {
		t.Fatal(err)
	}

	before := liveHeap()
	var reached atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	const length = 3 * mib // Above the SDK's Expect: 100-continue threshold.
	for i := range connections {
		wg.Go(func() {
			_, err := store.UploadPart(ctx, engine.UploadPartRequest{
				Ref: warmRef, UploadID: "upload", PartNumber: int32(i + 1), ContentLength: length,
				Body: &heldBody{length: length, holdAt: mib, reached: &reached, release: release},
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	deadline := time.Now().Add(60 * time.Second)
	for reached.Load() < int64(connections) {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("only %d of %d uploads started", reached.Load(), connections)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	perConnection := (liveHeap() - before) / int64(connections)
	close(release)
	wg.Wait()
	return perConnection
}

// TestMemPerConnectionBudget measures MemPerConnection on real TLS connections.
func TestMemPerConnectionBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("opens hundreds of TLS connections")
	}
	client := startEndpoint(t)
	for _, connections := range []int{32, engine.MaxActivePartsLimit} {
		perConnection := measurePerConnection(t, client, connections)
		t.Logf("%d connections: %.1f KiB each, budget %d KiB", connections, float64(perConnection)/1024, engine.MemPerConnection/engine.KiB)
		if perConnection > engine.MemPerConnection {
			t.Errorf("%d connections use %d bytes each, budget %d", connections, perConnection, engine.MemPerConnection)
		}
	}
}

// TestMemBaseBudget measures the idle engine with a warmed-up SDK client. It runs
// in a child process, so that no other test's heap can distort the baseline.
func TestMemBaseBudget(t *testing.T) {
	if os.Getenv(memBaseEnv) != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestMemBaseBudget$", "-test.v")
		child.Env = append(os.Environ(), memBaseEnv+"=1")
		output, err := child.CombinedOutput()
		t.Logf("%s", output)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	before := liveHeap()
	client := startEndpoint(t)
	store, err := journal.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := fakes3.NewSource()
	source.Set(srcPath, fakes3.SyntheticFile{Size: mib, Seed: seed})
	eng, err := engine.New(engine.Config{
		Store: s3x.NewStore(s3x.StaticClient(client)), Source: source,
		Sink: fakes3.NewSink(func(string) uint64 { return seed }), Journal: store, Prober: passProber{},
		Clock: engine.RealClock{}, Jitter: func() float64 { return 0.5 }, Settings: engine.DefaultSettings(),
		Retry: engine.DefaultRetryPolicy(), Timeouts: engine.DefaultTimeouts(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close(context.Background())
	ctx := context.Background()
	id, err := eng.Submit(ctx, engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: srcPath})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		all, err := eng.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if all[0].ID == id && all[0].State == engine.JobCompleted {
			break
		}
		if all[0].State.IsTerminal() || time.Now().After(deadline) {
			t.Fatalf("%+v %+v", all[0].State, all[0].Err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	base := liveHeap() - before
	t.Logf("idle engine with a warmed-up SDK client: %.2f MiB, budget %d MiB", float64(base)/float64(mib), engine.MemBase/engine.MiB)
	if base <= 0 || base > engine.MemBase {
		t.Fatalf("base memory is %d bytes, budget %d", base, engine.MemBase)
	}
}
