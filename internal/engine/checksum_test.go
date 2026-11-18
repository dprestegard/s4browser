package engine

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strconv"
	"testing"
)

func crcBase64(data []byte) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], crc32.ChecksumIEEE(data))
	return base64.StdEncoding.EncodeToString(raw[:])
}

func TestCompositeCRC32(t *testing.T) {
	first, second := []byte("first part"), []byte("second part")

	var concat []byte
	concat = binary.BigEndian.AppendUint32(concat, crc32.ChecksumIEEE(first))
	concat = binary.BigEndian.AppendUint32(concat, crc32.ChecksumIEEE(second))
	want := crcBase64(concat) + "-" + strconv.Itoa(2)

	got, err := CompositeCRC32([]string{crcBase64(first), crcBase64(second)})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestCompositeCRC32RejectsMalformedParts(t *testing.T) {
	for _, bad := range []string{"", "not base64!", "AAAAAAAA"} {
		if _, err := CompositeCRC32([]string{bad}); !errors.Is(err, ErrChecksumMismatch) {
			t.Errorf("%q: got %v", bad, err)
		}
	}
}
