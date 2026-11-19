// Package faultsuite holds the end-to-end fault suite of stage 2.1. It has no
// production code. Its tests drive the real engine through the real s3x adapter
// and the tuned transport against the HTTPS fake S3 (internal/fakes3), with the
// file journal and launch cleanup (journal.Recovery), and include killing a real
// process mid-transfer and relaunching.
//
// Every test runs inside the s4browser-test/fault/ prefix with the write guard
// on both the engine side and the fake server.
package faultsuite
