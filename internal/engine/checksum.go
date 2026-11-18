package engine

import (
	"encoding/base64"
	"fmt"
	"hash/crc32"
)

// CompositeCRC32 computes the checksum S3 reports for a COMPOSITE CRC32
// multipart upload: the CRC32 of the concatenated big-endian part checksums,
// base64 encoded, followed by "-" and the part count. partChecksums holds the
// base64 part values in part-number order.
func CompositeCRC32(partChecksums []string) (string, error) {
	hash := crc32.NewIEEE()
	for i, encoded := range partChecksums {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) != crc32.Size {
			return "", fmt.Errorf("%w: part %d has malformed CRC32 %q", ErrChecksumMismatch, i+1, encoded)
		}
		hash.Write(raw)
	}
	sum := base64.StdEncoding.EncodeToString(hash.Sum(nil))
	return fmt.Sprintf("%s-%d", sum, len(partChecksums)), nil
}
