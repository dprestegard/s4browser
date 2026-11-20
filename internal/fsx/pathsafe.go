package fsx

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// This file decides where an S3 key may be written on a Windows disk. It
// refuses, and never renames: a key that cannot be written exactly as named is
// reported with a reason and left alone. The rules apply on every OS, so the
// same key is judged the same way wherever the code runs.

// Reason codes of a PathError. The UI may group or filter refusals by code.
const (
	CodeAbsolute      = "absolute"       // The key starts with "/".
	CodeUNC           = "unc"            // The key looks like \\server\share or //server/share.
	CodeDrive         = "drive-path"     // The key starts with a drive letter such as "C:".
	CodeTraversal     = "traversal"      // A "." or ".." segment.
	CodeEmptySegment  = "empty-segment"  // "a//b", or an empty name.
	CodeBackslash     = "backslash"      // A backslash, which Windows reads as a separator.
	CodeForbiddenChar = "forbidden-char" // One of < > : " | ? *
	CodeControlChar   = "control-char"   // A control character.
	CodeTrailing      = "trailing-dot-space"
	CodeReserved      = "reserved-name" // CON, NUL, COM1 and the other device names, or an s4browser temp name.
	CodeNameTooLong   = "name-too-long" // One name over 255 UTF-16 units.
	CodeTooLong       = "too-long"      // The whole path is over MaxPath.
	CodeInvalidText   = "invalid-text"  // Not valid UTF-8.
	CodeCollision     = "collision"     // Same name as another item of the request, ignoring letter case, or a file where another item needs a folder.
	CodeExists        = "exists"        // The destination already exists locally. It is never replaced.
	CodeBlocked       = "blocked"       // A file is in the way of a folder the item needs.
	CodeLink          = "link"          // A symbolic link or junction lies on the way, or in an upload tree.
)

// MaxPath is the longest full path, in UTF-16 units, that a download may
// produce. It is Windows' MAX_PATH of 260 less the terminating NUL. Go's own
// file calls cope with longer paths, but Explorer, many media tools and the
// legacy Win32 calls do not, so a longer path is refused instead of written.
const MaxPath = 259

// MaxNameUnits is the longest single name, in UTF-16 units.
const MaxNameUnits = 255

// PathError is a refusal with a stable Code and a sentence for the user. It
// matches ErrUnsafePath, or ErrDestinationExists for CodeExists.
type PathError struct {
	Code string
	Msg  string
}

func (e *PathError) Error() string { return e.Msg }

// Unwrap lets errors.Is match the package's sentinels.
func (e *PathError) Unwrap() error {
	if e.Code == CodeExists {
		return ErrDestinationExists
	}
	return ErrUnsafePath
}

// CodeOf returns the Code of err if it is a PathError, else "".
func CodeOf(err error) string {
	var pe *PathError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func refuse(code, format string, args ...any) *PathError {
	return &PathError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// reservedStems are the device names Windows reserves, with or without an
// extension. COM1-9 and LPT1-9 (and their superscript forms) are added in
// reservedStem.
var reservedStems = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

func reservedStem(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	if reservedStems[stem] {
		return true
	}
	if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
		switch rest := stem[3:]; rest {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// utf16Len is the length of s in UTF-16 units, which is how Windows counts.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// CheckName checks one file or folder name.
func CheckName(name string) error {
	switch {
	case !utf8.ValidString(name):
		return refuse(CodeInvalidText, "the name %q is not valid text", name)
	case name == "":
		return refuse(CodeEmptySegment, "a name in the path is empty")
	case name == "." || name == "..":
		return refuse(CodeTraversal, "the path has a %q part, which could leave the destination folder", name)
	case strings.Contains(name, "/"):
		return refuse(CodeEmptySegment, "the name %q contains a slash", name)
	case strings.Contains(name, `\`):
		return refuse(CodeBackslash, "the name %q contains a backslash, which Windows reads as a folder separator", name)
	case strings.ContainsAny(name, `<>:"|?*`):
		return refuse(CodeForbiddenChar, "the name %q has a character Windows does not allow in a name (< > : \" | ? *)", name)
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return refuse(CodeControlChar, "the name %q has a control character, which Windows does not allow", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return refuse(CodeTrailing, "the name %q ends with a dot or a space, which Windows removes or refuses", name)
	case reservedStem(name):
		return refuse(CodeReserved, "the name %q is a reserved Windows device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9)", name)
	case utf16Len(name) > MaxNameUnits:
		return refuse(CodeNameTooLong, "the name %q is longer than the %d characters Windows allows in one name", name, MaxNameUnits)
	case IsTempPath(name):
		return refuse(CodeReserved, "the name %q is reserved for s4browser's temporary files", name)
	}
	return nil
}

// SplitKey splits a key, or the part of a key below a folder prefix, into its
// names. It refuses what is not a plain relative path: a leading slash, UNC and
// drive forms, backslashes, and empty, "." and ".." names. A trailing slash is
// an empty last name and is refused too, so callers handle folder keys first.
// It does not check the names for Windows rules: Placer.Place does.
func SplitKey(key string) ([]string, error) {
	switch {
	case !utf8.ValidString(key):
		return nil, refuse(CodeInvalidText, "the name %q is not valid text", key)
	case key == "":
		return nil, refuse(CodeEmptySegment, "the name is empty")
	case strings.HasPrefix(key, "//") || strings.HasPrefix(key, `\\`):
		return nil, refuse(CodeUNC, "the name starts like a network path (UNC), which cannot be written as a file name")
	case strings.HasPrefix(key, "/"):
		return nil, refuse(CodeAbsolute, "the name starts with a slash, so it would be an absolute path")
	case len(key) >= 2 && key[1] == ':' && (key[0]|0x20 >= 'a' && key[0]|0x20 <= 'z'):
		return nil, refuse(CodeDrive, "the name starts with a drive letter (%s), so it would be an absolute path", key[:2])
	case strings.Contains(key, `\`):
		return nil, refuse(CodeBackslash, "the name contains a backslash, which Windows reads as a folder separator")
	}
	return strings.Split(key, "/"), nil
}

// fold returns the form of a name in which two names that Windows treats as the
// same compare equal: letter case is ignored.
func foldRune(r rune) rune { return unicode.ToLower(unicode.ToUpper(r)) }

// Placer turns validated names into destination paths below one root folder and
// remembers what it has handed out, so a later item that would land on an
// earlier one is refused. One Placer serves one download request. It is not
// safe for concurrent use.
//
// Memory: it keeps one 64-bit hash per placed file and per distinct folder, about
// 40 bytes each, and the folders it has already seen on disk. It never keeps
// the names. A hash clash would refuse a name that does not collide (the safe
// direction), at odds of about n²/2⁶⁵ for n names.
type Placer struct {
	root    string
	files   map[uint64]struct{}
	dirs    map[uint64]struct{}
	onDisk  map[string]struct{} // Existing, ordinary folders already inspected.
	missing map[string]struct{} // Folders known not to exist yet.
}

// NewPlacer returns a Placer for root, which must be an absolute path to an
// existing folder. Root itself may be a link: the user chose it.
func NewPlacer(root string) (*Placer, error) {
	if !filepath.IsAbs(root) {
		return nil, refuse(CodeAbsolute, "the destination folder %q is not an absolute path", root)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the destination %q is not a folder that exists", root)
	}
	return &Placer{
		root: filepath.Clean(root), files: map[uint64]struct{}{}, dirs: map[uint64]struct{}{},
		onDisk: map[string]struct{}{}, missing: map[string]struct{}{},
	}, nil
}

// Root returns the destination folder.
func (p *Placer) Root() string { return p.root }

// Place returns the path for the file named by parts below the root, or a
// *PathError. Nothing is written. On success the name is reserved: a later
// Place of a name equal to it ignoring letter case, or needing it as a folder,
// fails. On failure nothing is reserved.
//
// It refuses, in this order: an unsafe name, a path longer than MaxPath (the
// temporary file beside it counts too), a collision with an earlier name, a
// link or a file on the way, and a destination that already exists.
func (p *Placer) Place(parts []string) (string, error) {
	if len(parts) == 0 {
		return "", refuse(CodeEmptySegment, "the name is empty")
	}
	for _, name := range parts {
		if err := CheckName(name); err != nil {
			return "", err
		}
	}
	final := filepath.Join(append([]string{p.root}, parts...)...)
	// Defence in depth: every name was checked, so this cannot fail.
	if rel, err := filepath.Rel(p.root, final); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || len(strings.Split(filepath.ToSlash(rel), "/")) != len(parts) {
		return "", refuse(CodeTraversal, "the path would leave the destination folder")
	}
	if n := utf16Len(final); n > MaxPath {
		return "", refuse(CodeTooLong, "the full path would be %d characters, over the Windows limit of %d: %s", n, MaxPath, final)
	}
	if n := utf16Len(TempPath(final, "0123456789abcdef")); n > MaxPath {
		return "", refuse(CodeTooLong, "the path of its temporary file would be %d characters, over the Windows limit of %d: %s", n, MaxPath, final)
	}

	// Collisions with names already placed.
	h := fnv.New64a()
	dirHashes := make([]uint64, 0, len(parts)-1)
	for i, name := range parts {
		for _, r := range name {
			var b [4]byte
			h.Write(b[:utf8.EncodeRune(b[:], foldRune(r))])
		}
		if i < len(parts)-1 {
			dirHashes = append(dirHashes, h.Sum64())
			h.Write([]byte{'/'})
		}
	}
	fileHash := h.Sum64()
	for _, d := range dirHashes {
		if _, ok := p.files[d]; ok {
			return "", refuse(CodeCollision, "another item in this request has the same name as the folder %q (letter case ignored), so a file and a folder would share one name", strings.Join(parts[:len(parts)-1], "/"))
		}
	}
	if _, ok := p.files[fileHash]; ok {
		return "", refuse(CodeCollision, "another item in this request has the same name %q when letter case is ignored, and Windows cannot hold both", strings.Join(parts, "/"))
	}
	if _, ok := p.dirs[fileHash]; ok {
		return "", refuse(CodeCollision, "another item in this request needs %q as a folder, so it cannot also be a file", strings.Join(parts, "/"))
	}

	// What is on disk already.
	dir := p.root
	for _, name := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, name)
		if _, ok := p.onDisk[dir]; ok {
			continue
		}
		if _, ok := p.missing[dir]; ok {
			break
		}
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			p.missing[dir] = struct{}{}
		case err != nil:
			return "", refuse(CodeBlocked, "cannot inspect %s: %v", dir, unwrapPathErr(err))
		case info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			return "", refuse(CodeLink, "%s is a link or junction, and a download will not write through it", dir)
		case !info.IsDir():
			return "", refuse(CodeBlocked, "%s is a file, so it cannot hold a folder of the same name", dir)
		default:
			p.onDisk[dir] = struct{}{}
			continue
		}
		break
	}
	if _, err := os.Lstat(final); err == nil {
		return "", refuse(CodeExists, "%s already exists and is not replaced", final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", refuse(CodeBlocked, "cannot inspect %s: %v", final, unwrapPathErr(err))
	}

	for _, d := range dirHashes {
		p.dirs[d] = struct{}{}
	}
	p.files[fileHash] = struct{}{}
	return final, nil
}

func unwrapPathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// MakeParents creates the folders a path from Place needs, one level at a time,
// and never through a link. It creates nothing if every part exists. Call it
// only for a path Place accepted.
func (p *Placer) MakeParents(final string) error {
	rel, err := filepath.Rel(p.root, filepath.Dir(final))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return refuse(CodeTraversal, "the path would leave the destination folder")
	}
	if rel == "." {
		return nil
	}
	dir := p.root
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, name)
		if _, ok := p.onDisk[dir]; ok {
			continue
		}
		err := os.Mkdir(dir, 0o777)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create folder %s: %w", dir, unwrapPathErr(err))
		}
		info, serr := os.Lstat(dir)
		if serr != nil || !info.IsDir() || info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return refuse(CodeLink, "%s is not an ordinary folder", dir)
		}
		delete(p.missing, dir)
		p.onDisk[dir] = struct{}{}
	}
	return nil
}
