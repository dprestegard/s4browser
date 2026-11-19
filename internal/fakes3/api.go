package fakes3

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// This file is the test-facing control surface of Server.

// AddRule installs a fault rule. Rules are tried in installation order and the
// first match fires.
func (s *Server) AddRule(r Rule) RuleID { return s.core.rules.add(r) }

// RemoveRule uninstalls a rule.
func (s *Server) RemoveRule(id RuleID) { s.core.rules.remove(id) }

// ClearRules uninstalls every rule. Attempt counters are kept.
func (s *Server) ClearRules() { s.core.rules.clear() }

// RuleFired reports how many times a rule has fired.
func (s *Server) RuleFired(id RuleID) int { return s.core.rules.fired(id) }

// Attempts counts the requests started for one operation, bucket, key and part
// number (0 for operations without a part). Requests refused by the guard are
// not counted. A test proves that a finished part was not sent again by
// asserting Attempts(OpUploadPart, ...) == 1.
func (s *Server) Attempts(op Op, bucket, key string, part int32) int {
	return s.core.rules.attemptsOf(op, bucket, key, part)
}

// Requests returns a copy of the log of finished requests, in completion order.
func (s *Server) Requests() []RequestRecord {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	return append([]RequestRecord(nil), s.core.log...)
}

// Finished counts finished requests matching op, bucket, key and part. Empty
// strings and part 0 match anything.
func (s *Server) Finished(op Op, bucket, key string, part int32) int {
	n := 0
	for _, r := range s.Requests() {
		if (op == "" || r.Op == op) && (bucket == "" || r.Bucket == bucket) && (key == "" || r.Key == key) && (part == 0 || r.Part == part) {
			n++
		}
	}
	return n
}

// RefusedWrites counts requests the server-side guard rejected.
func (s *Server) RefusedWrites() int {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	return s.core.refused
}

// AddBucket makes a bucket exist in Region.
func (s *Server) AddBucket(name string) { s.AddBucketInRegion(name, Region) }

// AddBucketInRegion makes a bucket exist in the given region. An existing
// bucket keeps its objects and moves to region.
func (s *Server) AddBucketInRegion(name, region string) {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	if b := s.core.buckets[name]; b != nil {
		b.region = region
		return
	}
	s.core.buckets[name] = newBucket(region, s.core.clock.Now())
}

// PutListed creates a metadata-only object for listing tests: it has the given
// size and modification time, and reading its data is an error.
func (s *Server) PutListed(bucket, key string, size int64, modified time.Time) error {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b, e := s.core.bucketLocked(bucket)
	if e != nil {
		return e
	}
	if e := validKey(key); e != nil {
		return e
	}
	sum := md5.Sum(fmt.Appendf(nil, "listed|%s|%d", key, size))
	b.putLocked(key, &object{size: size, etag: hex.EncodeToString(sum[:]), modified: modified})
	return nil
}

// PutSynthetic creates an object whose content is Pattern{seed} of the given
// size. Nothing is stored beyond this metadata, so size may be many terabytes.
// Its HEAD response carries no checksum, because computing one would read it.
func (s *Server) PutSynthetic(bucket, key string, seed uint64, size int64) error {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b, e := s.core.bucketLocked(bucket)
	if e != nil {
		return e
	}
	sum := md5.Sum(fmt.Appendf(nil, "synthetic|%d|%d", seed, size))
	b.putLocked(key, &object{
		size: size, etag: hex.EncodeToString(sum[:]), modified: s.core.clock.Now(),
		seed: seed, seedKnown: true, parts: []partMeta{{size: size, etag: hex.EncodeToString(sum[:])}},
	})
	return nil
}

// ObjectState describes a stored object.
type ObjectState struct {
	Size  int64
	ETag  string // quoted
	Parts []PartState
	// Multipart is true for objects made by CompleteMultipartUpload.
	Multipart bool
	// CRC32 is the checksum S3 would report: composite ("<b64>-<n>") for
	// multipart objects. Empty when unknown.
	CRC32    string
	Modified time.Time
	// ContentKnown is true once the content is bound to a seed, by PutSynthetic
	// or a successful VerifyObject.
	ContentKnown bool
	Seed         uint64
}

// PartState is the recorded metadata of one part.
type PartState struct {
	Size  int64
	CRC32 uint32
	ETag  string
}

// Object returns an object's metadata.
func (s *Server) Object(bucket, key string) (ObjectState, bool) {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b := s.core.buckets[bucket]
	if b == nil || b.objects[key] == nil {
		return ObjectState{}, false
	}
	o := b.objects[key]
	st := ObjectState{Size: o.size, ETag: quote(o.etag), Multipart: o.multipart, CRC32: o.crc, Modified: o.modified, ContentKnown: o.seedKnown, Seed: o.seed}
	for _, p := range o.parts {
		st.Parts = append(st.Parts, PartState{Size: p.size, CRC32: p.crc, ETag: quote(p.etag)})
	}
	return st, true
}

// ObjectCount returns how many objects a bucket holds.
func (s *Server) ObjectCount(bucket string) int {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	if b := s.core.buckets[bucket]; b != nil {
		return len(b.objects)
	}
	return 0
}

// UploadState describes an in-progress multipart upload.
type UploadState struct {
	Key      string
	UploadID string
	Created  time.Time
	// Parts lists the stored part numbers in ascending order.
	Parts []int32
}

// ActiveUploads lists a bucket's in-progress uploads ordered by key and ID.
func (s *Server) ActiveUploads(bucket string) []UploadState {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b := s.core.buckets[bucket]
	if b == nil {
		return nil
	}
	var res []UploadState
	for _, u := range b.uploads {
		st := UploadState{Key: u.key, UploadID: u.id, Created: u.created}
		for n := range u.parts {
			st.Parts = append(st.Parts, n)
		}
		sort.Slice(st.Parts, func(i, j int) bool { return st.Parts[i] < st.Parts[j] })
		res = append(res, st)
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Key != res[j].Key {
			return res[i].Key < res[j].Key
		}
		return res[i].UploadID < res[j].UploadID
	})
	return res
}

// VerifyObject checks that an object's recorded part checksums match
// Pattern{seed}, which is what a synthetic Source with that seed uploads. Every
// stride-th part is recomputed (stride <= 1 checks all, and the last part is
// always checked). On success the object's content is bound to the seed, so
// ranged GETs return the verified bytes.
func (s *Server) VerifyObject(bucket, key string, seed uint64, stride int) error {
	s.core.mu.Lock()
	b := s.core.buckets[bucket]
	var o *object
	if b != nil {
		o = b.objects[key]
	}
	if o == nil {
		s.core.mu.Unlock()
		return fmt.Errorf("fakes3: no object %s/%s", bucket, key)
	}
	parts := append([]partMeta(nil), o.parts...)
	changes := o.changes
	size := o.size
	if o.seedKnown && o.seed != seed {
		s.core.mu.Unlock()
		return fmt.Errorf("fakes3: object %s is bound to seed %d, not %d", key, o.seed, seed)
	}
	s.core.mu.Unlock()

	offsets := make([]int64, len(parts))
	var total int64
	for i, p := range parts {
		offsets[i] = total
		total += p.size
	}
	if total != size {
		return fmt.Errorf("fakes3: object %s parts total %d bytes, size is %d", key, total, size)
	}
	if stride < 1 {
		stride = 1
	}
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		first   error
		next    = make(chan int)
	)
	pt := Pattern{Seed: seed}
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if got := pt.CRC32(offsets[i], parts[i].size); got != parts[i].crc {
					errOnce.Do(func() {
						first = fmt.Errorf("fakes3: %s part %d at offset %d: stored CRC32 %08x, expected %08x for seed %d",
							key, i+1, offsets[i], parts[i].crc, got, seed)
					})
				}
			}
		}()
	}
	for i := range parts {
		if i%stride == 0 || i == len(parts)-1 {
			next <- i
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return first
	}

	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	if cur := b.objects[key]; cur == o && o.changes == changes {
		o.seed, o.seedKnown = seed, true
	}
	return nil
}
