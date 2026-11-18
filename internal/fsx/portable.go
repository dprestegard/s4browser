package fsx

import (
	"errors"
	"fmt"
	"os"
)

// DefaultLead is the MaxLead of the strategies that need one. It is the figure
// chosen from the stage 0.5 measurements, see docs/progress.md.
const DefaultLead = 2 << 30

// fileTarget is a plain os.File. WriteAt is safe for concurrent use on
// non-overlapping ranges.
type fileTarget struct{ f *os.File }

func (t *fileTarget) WriteAt(p []byte, off int64) (int, error) { return t.f.WriteAt(p, off) }
func (t *fileTarget) Sync() error                              { return t.f.Sync() }
func (t *fileTarget) Close() error                             { return t.f.Close() }

// createExclusive makes a new file, failing if it exists.
func createExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
}

// portableStrategy works on any filesystem. With extend false the file grows as
// it is written, which is the sequential-lead behaviour. With extend true the end
// of file is set to the final size at Create, which makes the file sparse on
// filesystems that support holes and zero-filled elsewhere.
type portableStrategy struct {
	name   string
	extend bool
	lead   int64
}

func (s portableStrategy) Name() string   { return s.name }
func (s portableStrategy) MaxLead() int64 { return s.lead }

func (s portableStrategy) Create(path string, size int64) (WriteTarget, error) {
	if size < 0 {
		return nil, fmt.Errorf("fsx: negative size %d", size)
	}
	f, err := createExclusive(path)
	if err != nil {
		return nil, err
	}
	if s.extend && size > 0 {
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, err
		}
	}
	return &fileTarget{f}, nil
}

// ErrUnsupportedStrategy means a strategy is unknown or cannot run on this system.
var ErrUnsupportedStrategy = errors.New("fsx: write strategy not available")

// Names lists every strategy constant, in the order stage 0.5 compared them.
func Names() []string {
	return []string{StrategySequentialLead, StrategyPreallocate, StrategySparse, StrategyValidData}
}
