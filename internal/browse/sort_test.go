package browse

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestCompareNames(t *testing.T) {
	ordered := []string{
		"", "0", "00", "01", "1", "2", "10", "a", "A1", "a2", "a10", "B", "b", "shot2.exr", "Shot10.exr",
		"shot100.exr", "x", "~", "ä",
	}
	for i := range ordered {
		for j := range ordered {
			got := CompareNames(ordered[i], ordered[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if sign(got) != want {
				t.Errorf("CompareNames(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestIncrementalSortMatchesFullSort adds rows in random chunks, asks for each
// order after every chunk, and compares the result with a fresh full sort.
func TestIncrementalSortMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	l := &Listing{prefix: "p/", classIdx: map[string]uint8{}, views: map[SortSpec][]int32{}}
	classes := []string{"STANDARD", "GLACIER", "STANDARD_IA", "DEEP_ARCHIVE"}
	specs := []SortSpec{}
	for _, f := range []SortField{SortName, SortSize, SortModified, SortClass} {
		specs = append(specs, SortSpec{f, false}, SortSpec{f, true})
	}
	n := 0
	for range 40 {
		for range rng.IntN(300) {
			n++
			r := row{key: "p/" + randomName(rng, n)}
			if rng.IntN(10) == 0 {
				r.key += "/"
				r.folder = true
			} else {
				r.size = int64(rng.IntN(50)) // many ties
				r.mod = int64(rng.IntN(20))
				r.class = l.classLocked(classes[rng.IntN(len(classes))])
			}
			l.rows = append(l.rows, r)
		}
		for _, spec := range specs {
			got := l.viewLocked(spec)
			want := make([]int32, len(l.rows))
			for i := range want {
				want[i] = int32(i)
			}
			slices.SortFunc(want, l.compareFunc(spec))
			if !slices.Equal(got, want) {
				t.Fatalf("%v after %d rows: incremental order differs from a full sort", spec, len(l.rows))
			}
		}
	}
}

func randomName(rng *rand.Rand, unique int) string {
	const alphabet = "aAbB0123456789-_."
	b := make([]byte, 1+rng.IntN(8))
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b) + "#" + string(rune('a'+unique%26)) + itoa(unique)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
