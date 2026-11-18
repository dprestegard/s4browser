package engine

import (
	"errors"
	"testing"
)

func TestPlanParts(t *testing.T) {
	const tib = 1024 * GiB
	tests := []struct {
		name          string
		size          int64
		preferred     int64
		wantPartSize  int64
		wantPartCount int
	}{
		{"empty object", 0, 0, DefaultPartSize, 0},
		{"one byte", 1, 0, DefaultPartSize, 1},
		{"default exact multiple", 128 * MiB, 0, 64 * MiB, 2},
		{"default with remainder", 128*MiB + 1, 0, 64 * MiB, 3},
		{"preferred below minimum is clamped", 100 * MiB, 1 * MiB, 5 * MiB, 20},
		{"preferred above maximum is clamped", 6 * GiB, 6 * GiB, 5 * GiB, 2},
		{"preferred is aligned up", 100 * MiB, 10*MiB + 1, 11 * MiB, 10},
		{"last size using default", 10_000 * 64 * MiB, 0, 64 * MiB, 10_000},
		{"one byte past default capacity", 10_000*64*MiB + 1, 0, 65 * MiB, 9847},
		{"2 TiB raises part size", 2 * tib, 0, 210 * MiB, 9987},
		{"2 TiB keeps larger preference", 2 * tib, 512 * MiB, 512 * MiB, 4096},
		{"maximum object", MaxObjectSize, 0, MaxPartSize, MaxPartCount},
		{"alignment never exceeds maximum", MaxObjectSize - 1, 0, MaxPartSize, MaxPartCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PlanParts(tt.size, tt.preferred)
			if err != nil {
				t.Fatal(err)
			}
			if got.PartSize != tt.wantPartSize || got.PartCount != tt.wantPartCount {
				t.Fatalf("got size %d count %d, want size %d count %d",
					got.PartSize, got.PartCount, tt.wantPartSize, tt.wantPartCount)
			}
		})
	}
}

func TestPlanPartsRejectsInvalidSizes(t *testing.T) {
	if _, err := PlanParts(MaxObjectSize+1, 0); !errors.Is(err, ErrObjectTooLarge) {
		t.Errorf("oversized object: got %v", err)
	}
	if _, err := PlanParts(-1, 0); !errors.Is(err, ErrInvalidSize) {
		t.Errorf("negative object: got %v", err)
	}
	if _, err := PlanParts(1, -1); !errors.Is(err, ErrInvalidSize) {
		t.Errorf("negative preferred: got %v", err)
	}
}

func TestPartPlanCoversObjectExactly(t *testing.T) {
	sizes := []int64{1, 5 * MiB, 64*MiB + 1, 1000*MiB + 123, 2048 * GiB, MaxObjectSize}
	for _, size := range sizes {
		plan, err := PlanParts(size, 0)
		if err != nil {
			t.Fatal(err)
		}
		if plan.PartCount > MaxPartCount {
			t.Fatalf("size %d: %d parts", size, plan.PartCount)
		}
		last := plan.Part(plan.PartCount - 1)
		if got := last.Offset + last.Length; got != size {
			t.Fatalf("size %d: parts end at %d", size, got)
		}
		if last.Length <= 0 || last.Length > plan.PartSize {
			t.Fatalf("size %d: last part length %d", size, last.Length)
		}
		if last.Number != int32(plan.PartCount) {
			t.Fatalf("size %d: last part number %d", size, last.Number)
		}
	}
}

func TestUseSinglePut(t *testing.T) {
	tests := map[int64]bool{0: true, SinglePutThreshold: true, SinglePutThreshold + 1: false}
	for size, want := range tests {
		if got := UseSinglePut(size); got != want {
			t.Errorf("UseSinglePut(%d) = %v, want %v", size, got, want)
		}
	}
	if SinglePutThreshold > MaxSinglePutSize {
		t.Error("threshold exceeds the S3 single PUT limit")
	}
}
