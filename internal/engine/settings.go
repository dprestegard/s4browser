package engine

import "fmt"

// Settings are the user-tunable limits. All of them can change while transfers
// run. See Engine.UpdateSettings for when each change takes effect.
type Settings struct {
	// MaxActiveFiles limits jobs holding an active-file slot.
	MaxActiveFiles int
	// MaxPartsPerFile limits in-flight parts of one job.
	MaxPartsPerFile int
	// MaxActiveParts limits in-flight parts across all jobs.
	MaxActiveParts int
	// PartSize is the preferred part size, or 0 for DefaultPartSize.
	PartSize int64
	// UploadBytesPerSec caps total upload rate. 0 means unlimited.
	UploadBytesPerSec int64
	// DownloadBytesPerSec caps total download rate. 0 means unlimited.
	DownloadBytesPerSec int64
}

// DefaultSettings returns the settings used before the user changes anything.
func DefaultSettings() Settings {
	return Settings{
		MaxActiveFiles:  DefaultActiveFiles,
		MaxPartsPerFile: DefaultPartsPerFile,
		MaxActiveParts:  DefaultActiveParts,
		PartSize:        DefaultPartSize,
	}
}

// Validate returns an error wrapping ErrInvalidSettings for out-of-range values.
func (s Settings) Validate() error {
	switch {
	case s.MaxActiveFiles < MinActiveFiles || s.MaxActiveFiles > MaxActiveFilesLimit:
		return fmt.Errorf("%w: MaxActiveFiles %d", ErrInvalidSettings, s.MaxActiveFiles)
	case s.MaxPartsPerFile < MinPartsPerFile || s.MaxPartsPerFile > MaxPartsPerFileLimit:
		return fmt.Errorf("%w: MaxPartsPerFile %d", ErrInvalidSettings, s.MaxPartsPerFile)
	case s.MaxActiveParts < MinActiveParts || s.MaxActiveParts > MaxActivePartsLimit:
		return fmt.Errorf("%w: MaxActiveParts %d", ErrInvalidSettings, s.MaxActiveParts)
	case s.PartSize != 0 && (s.PartSize < MinPartSize || s.PartSize > MaxPartSize):
		return fmt.Errorf("%w: PartSize %d", ErrInvalidSettings, s.PartSize)
	case s.UploadBytesPerSec < 0 || s.DownloadBytesPerSec < 0:
		return fmt.Errorf("%w: negative bandwidth limit", ErrInvalidSettings)
	}
	return nil
}

// Memory budget terms, in bytes of live Go heap. Tests in internal/engine and
// internal/s3x measure them. Update these constants and docs/engine-design.md
// together if a term is exceeded.
const (
	// MemBase covers the scheduler, watchdog, progress and SDK client state, plus
	// the short-lived request built by CompleteMultipartUpload. Measured idle: 0.21 MiB.
	MemBase = 8 * MiB
	// MemPerConnection covers TLS records, transport buffers and request state.
	// Measured: 200 to 217 KiB per in-flight upload.
	MemPerConnection = 256 * KiB
	// MemPerPartEntry is one entry of an active job's part table.
	MemPerPartEntry = 96
	// MemPerJobRecord is one tracked job that has no part table.
	MemPerJobRecord = 2 * KiB
	// MemoryAssertGCFactor allows for garbage awaiting collection at GOGC=100.
	MemoryAssertGCFactor = 2
)

// MemoryBound returns the peak live heap the engine may use:
//
//	MemBase
//	+ (MaxActiveParts + MaxActiveFiles) * MemPerConnection
//	+ MaxActiveParts * DownloadBufferSize
//	+ MaxActiveFiles * MaxPartCount * MemPerPartEntry
//	+ trackedJobs * MemPerJobRecord
//
// Part size and object size are deliberately absent: part bodies are streamed
// from the source and never buffered.
func MemoryBound(s Settings, trackedJobs int) int64 {
	parts, files := int64(s.MaxActiveParts), int64(s.MaxActiveFiles)
	return MemBase +
		(parts+files)*MemPerConnection +
		parts*DownloadBufferSize +
		files*MaxPartCount*MemPerPartEntry +
		int64(trackedJobs)*MemPerJobRecord
}
