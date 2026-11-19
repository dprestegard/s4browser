package browse

import (
	"cmp"
	"slices"
	"strings"
)

// viewLocked returns the row order for spec, covering every row loaded so far.
// Each order is kept and extended as pages arrive: only the new rows are sorted,
// then merged in, so a window request during loading costs O(new log new + n)
// instead of a full sort.
func (l *Listing) viewLocked(spec SortSpec) []int32 {
	perm := l.views[spec]
	n := len(l.rows)
	if len(perm) == n {
		return perm
	}
	less := l.compareFunc(spec)
	added := make([]int32, 0, n-len(perm))
	for i := len(perm); i < n; i++ {
		added = append(added, int32(i))
	}
	slices.SortFunc(added, less)
	merged := make([]int32, 0, n)
	i, j := 0, 0
	for i < len(perm) && j < len(added) {
		// Ties keep the existing row first, so a sorted position never moves
		// above rows that were already there.
		if less(added[j], perm[i]) < 0 {
			merged = append(merged, added[j])
			j++
		} else {
			merged = append(merged, perm[i])
			i++
		}
	}
	merged = append(merged, perm[i:]...)
	merged = append(merged, added[j:]...)
	l.views[spec] = merged
	return merged
}

// compareFunc orders rows: folders first, then by the field (reversed when
// Desc), then by name ascending.
func (l *Listing) compareFunc(spec SortSpec) func(a, b int32) int {
	rows, skip := l.rows, len(l.prefix)
	classes := l.classes
	byName := func(a, b *row) int { return CompareNames(a.key[skip:], b.key[skip:]) }
	var field func(a, b *row) int
	switch spec.Field {
	case SortSize:
		field = func(a, b *row) int { return cmp.Compare(a.size, b.size) }
	case SortModified:
		field = func(a, b *row) int { return cmp.Compare(a.mod, b.mod) }
	case SortClass:
		field = func(a, b *row) int { return strings.Compare(classes[a.class], classes[b.class]) }
	default:
		field = byName
	}
	return func(ia, ib int32) int {
		a, b := &rows[ia], &rows[ib]
		if a.folder != b.folder {
			if a.folder {
				return -1
			}
			return 1
		}
		// Folders have no size, date or class, so those orders list them by
		// name, ascending.
		if a.folder && spec.Field != SortName && spec.Field != "" {
			return byName(a, b)
		}
		c := field(a, b)
		if spec.Desc {
			c = -c
		}
		if c == 0 {
			c = byName(a, b)
		}
		return c
	}
}

// CompareNames orders names the way Explorer does: ASCII letters ignore case,
// and runs of digits compare by value, so "shot2" comes before "shot10". Names
// that are still equal (such as "A" and "a", or "07" and "7") are ordered by
// their bytes, so the order is total and stable.
func CompareNames(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ca, cb := a[i], b[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for si < len(a) && a[si] == '0' {
				si++
			}
			for sj < len(b) && b[sj] == '0' {
				sj++
			}
			ei, ej := si, sj
			for ei < len(a) && isDigit(a[ei]) {
				ei++
			}
			for ej < len(b) && isDigit(b[ej]) {
				ej++
			}
			if c := cmp.Compare(ei-si, ej-sj); c != 0 {
				return c
			}
			if c := strings.Compare(a[si:ei], b[sj:ej]); c != 0 {
				return c
			}
			i, j = ei, ej
			continue
		}
		if c := cmp.Compare(lowerASCII(ca), lowerASCII(cb)); c != 0 {
			return c
		}
		i++
		j++
	}
	if c := cmp.Compare(len(a)-i, len(b)-j); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
