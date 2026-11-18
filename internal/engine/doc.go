// Package engine holds the transfer engine contracts: job and part state
// machines, part sizing, the error taxonomy, retry policy, memory bound and the
// seams (object store, source, sink, clock) that the scheduler is written
// against.
//
// docs/engine-design.md is the normative description. This package must stay
// free of the AWS SDK and of OS-specific code: internal/s3x adapts the SDK to
// ObjectStore, and internal/fsx owns NTFS behaviour.
package engine
