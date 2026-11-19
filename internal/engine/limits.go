package engine

import "time"

// Byte size units.
const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
)

// S3 service limits, checked on 2026-11-17 against
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html
const (
	// MinPartSize is the smallest allowed part, except for the last part.
	MinPartSize = 5 * MiB
	// MaxPartSize is the largest allowed part.
	MaxPartSize = 5 * GiB
	// MaxPartCount is the largest part count of one multipart upload.
	MaxPartCount = 10_000
	// MaxObjectSize is the largest object, which AWS documents as 48.8 TiB.
	// It equals MaxPartSize * MaxPartCount.
	MaxObjectSize = MaxPartSize * MaxPartCount
	// MaxSinglePutSize is the largest object accepted by a single PutObject.
	MaxSinglePutSize = 5 * GiB
)

// Part sizing policy.
const (
	// DefaultPartSize is used when the user has not chosen a part size.
	DefaultPartSize = 64 * MiB
	// PartSizeAlign is the boundary every computed part size is rounded up to.
	PartSizeAlign = 1 * MiB
	// SinglePutThreshold is the largest file sent with one PutObject. Larger
	// files use multipart upload. The value caps the data re-sent by one retry.
	SinglePutThreshold = 64 * MiB
	// JournalTempMinSize is the smallest download whose temp file is journaled.
	// Skipping small files avoids two fsyncs per file in large folder downloads.
	JournalTempMinSize = SinglePutThreshold + 1
)

// Settings bounds. Settings.Validate enforces them.
const (
	MinActiveFiles       = 1
	MaxActiveFilesLimit  = 64
	MinPartsPerFile      = 1
	MaxPartsPerFileLimit = 128
	MinActiveParts       = 1
	MaxActivePartsLimit  = 256
)

// Settings defaults.
const (
	DefaultActiveFiles  = 4
	DefaultPartsPerFile = 16
	DefaultActiveParts  = 32
)

// Data path tuning.
const (
	// MeterQuantum is how many bytes an attempt accumulates locally before it
	// charges the limiter and publishes progress.
	MeterQuantum = 256 * KiB
	// DownloadBufferSize is the pooled buffer each download attempt reads into.
	DownloadBufferSize = 1 * MiB
	// ProgressInterval is the batch period for Observer delivery.
	ProgressInterval = 200 * time.Millisecond
	// WatchdogTick is the scan period of the stall watchdog.
	WatchdogTick = 1 * time.Second
	// MaxBufferedTransitions bounds transitions waiting for the Observer. On
	// overflow the oldest are dropped and ProgressBatch.Resync is set.
	MaxBufferedTransitions = 4096
	// DefaultSendBuffer is the socket send buffer internal/s3x gives every
	// connection, and the default of Config.SendBuffer. When an upload body has
	// been read, up to this much can still be waiting in the socket.
	DefaultSendBuffer = 4 * MiB
	// MaxDrainAllowance caps the time the watchdog adds to Timeouts.Response for
	// a send buffer to drain. See AttemptActivity.SetDrain.
	MaxDrainAllowance = 20 * time.Minute
)

// Timeouts holds the deadlines applied to requests. Zero values are invalid;
// start from DefaultTimeouts.
type Timeouts struct {
	// Stall cancels an attempt that moves no bytes for this long while in
	// AttemptSend or AttemptReceive.
	Stall time.Duration
	// Response cancels an attempt that spends this long in AttemptAwaitResponse.
	Response time.Duration
	// Control is the deadline of one Create, Abort or Head request.
	Control time.Duration
	// Complete is the deadline of one CompleteMultipartUpload request, which S3
	// may take minutes to answer on large objects.
	Complete time.Duration
	// ShutdownAbort is the total time Engine.Close spends aborting uploads.
	ShutdownAbort time.Duration
	// ProbeMin and ProbeMax bound the credential probe interval, which doubles
	// after each failed probe.
	ProbeMin time.Duration
	ProbeMax time.Duration
}

// DefaultTimeouts returns the production deadlines.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Stall:         30 * time.Second,
		Response:      60 * time.Second,
		Control:       60 * time.Second,
		Complete:      20 * time.Minute,
		ShutdownAbort: 10 * time.Second,
		ProbeMin:      5 * time.Second,
		ProbeMax:      60 * time.Second,
	}
}
