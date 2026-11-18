package engine

import "fmt"

// PartPlan is the fixed division of an object into parts. Every part is
// PartSize bytes except the last, which holds the remainder.
type PartPlan struct {
	ObjectSize int64
	PartSize   int64
	PartCount  int
}

// PartRange is one part's position in the object.
type PartRange struct {
	// Number is the 1-based S3 part number.
	Number int32
	Offset int64
	Length int64
}

// PlanParts applies the part sizing rule to both uploads and downloads:
//
//  1. Start from preferred, or DefaultPartSize when preferred is 0.
//  2. Clamp to [MinPartSize, MaxPartSize].
//  3. Raise to ceil(objectSize / MaxPartCount) if that is larger.
//  4. Round up to a multiple of PartSizeAlign, capped at MaxPartSize.
//
// A zero-byte object yields a plan with no parts. Negative preferred or object
// sizes return ErrInvalidSize, and objects above MaxObjectSize return
// ErrObjectTooLarge.
func PlanParts(objectSize, preferred int64) (PartPlan, error) {
	if objectSize < 0 || preferred < 0 {
		return PartPlan{}, fmt.Errorf("%w: object %d, preferred %d", ErrInvalidSize, objectSize, preferred)
	}
	if objectSize > MaxObjectSize {
		return PartPlan{}, fmt.Errorf("%w: %d bytes", ErrObjectTooLarge, objectSize)
	}

	size := preferred
	if size == 0 {
		size = DefaultPartSize
	}
	size = min(max(size, MinPartSize), MaxPartSize)
	size = max(size, ceilDiv(objectSize, MaxPartCount))
	size = min(ceilDiv(size, PartSizeAlign)*PartSizeAlign, MaxPartSize)

	return PartPlan{
		ObjectSize: objectSize,
		PartSize:   size,
		PartCount:  int(ceilDiv(objectSize, size)),
	}, nil
}

// Part returns the range of the part at zero-based index i. It panics when i is
// out of range, because the scheduler only iterates over planned parts.
func (p PartPlan) Part(i int) PartRange {
	if i < 0 || i >= p.PartCount {
		panic(fmt.Sprintf("engine: part index %d out of range [0,%d)", i, p.PartCount))
	}
	offset := int64(i) * p.PartSize
	return PartRange{
		Number: int32(i + 1),
		Offset: offset,
		Length: min(p.PartSize, p.ObjectSize-offset),
	}
}

// UseSinglePut reports whether an upload of size bytes uses one PutObject
// instead of a multipart upload. Zero-byte files always do.
func UseSinglePut(size int64) bool {
	return size <= SinglePutThreshold
}

func ceilDiv(a, b int64) int64 {
	if a == 0 {
		return 0
	}
	return (a-1)/b + 1
}
