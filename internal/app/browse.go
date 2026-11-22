package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"s4browser/internal/browse"
)

// browseTimeout covers ListBuckets and region detection, including a
// credential_process helper that may take up to a minute.
const browseTimeout = 75 * time.Second

// openBrowse opens the browse service. Its saved buckets live next to the
// profiles in the app's config folder.
func (a *App) openBrowse() (*browse.Service, error) {
	if a.profiles == nil {
		return nil, errors.New("the profile store is not available")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	o := browse.Options{
		Dir:         filepath.Join(dir, "s4browser"),
		Credentials: a.profiles.Provider,
		Notify: func(s browse.Snapshot) {
			a.emit("s4:listing", s)
		},
	}
	if err := devBrowseOptions(&o); err != nil {
		return nil, err
	}
	return browse.New(o)
}

func (a *App) browser() (*browse.Service, error) {
	if a.browse == nil {
		return nil, errors.New("browsing is not available: " + a.browseErr)
	}
	return a.browse, nil
}

// Buckets lists the profile's buckets, including those added by name.
func (a *App) Buckets(profileID string) (browse.BucketList, error) {
	s, err := a.browser()
	if err != nil {
		return browse.BucketList{}, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, browseTimeout)
	defer cancel()
	return s.Buckets(ctx, devProfile(profileID))
}

// AddBucket remembers a bucket for the profile. region is used only if the
// bucket's region cannot be detected.
func (a *App) AddBucket(profileID, name, region string) (browse.AddResult, error) {
	s, err := a.browser()
	if err != nil {
		return browse.AddResult{}, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, browseTimeout)
	defer cancel()
	return s.AddBucket(ctx, devProfile(profileID), name, region)
}

// RemoveBucket forgets a bucket that was added by name.
func (a *App) RemoveBucket(profileID, name string) error {
	s, err := a.browser()
	if err != nil {
		return err
	}
	return s.RemoveBucket(devProfile(profileID), name)
}

// OpenListing starts listing a prefix. Progress arrives as "s4:listing" events,
// and rows are fetched with ListingWindow.
func (a *App) OpenListing(profileID, bucket, prefix string) (browse.Snapshot, error) {
	s, err := a.browser()
	if err != nil {
		return browse.Snapshot{}, err
	}
	return s.OpenListing(devProfile(profileID), bucket, prefix)
}

// ListingWindow returns up to limit rows from offset in the given order.
func (a *App) ListingWindow(id string, field string, desc bool, offset, limit int) (browse.Window, error) {
	s, err := a.browser()
	if err != nil {
		return browse.Window{}, err
	}
	return s.Window(id, browse.SortSpec{Field: browse.SortField(field), Desc: desc}, offset, limit)
}

// CloseListing frees a listing the UI no longer shows.
func (a *App) CloseListing(id string) {
	if s, err := a.browser(); err == nil {
		s.CloseListing(id)
	}
}

// ListingSummary totals a selection made of inclusive index ranges in the
// given sort order, without sending the rows to the UI.
func (a *App) ListingSummary(id string, field string, desc bool, ranges [][2]int) (browse.Summary, error) {
	s, err := a.browser()
	if err != nil {
		return browse.Summary{}, err
	}
	return s.Summarize(id, browse.SortSpec{Field: browse.SortField(field), Desc: desc}, ranges)
}

// ListingKeys returns the keys of a selection of at most browse.MaxLocate rows,
// made on this version of the listing. The UI keeps them across a reload.
func (a *App) ListingKeys(id string, field string, desc bool, version uint64, ranges [][2]int) ([]string, error) {
	s, err := a.browser()
	if err != nil {
		return nil, err
	}
	spec := browse.SortSpec{Field: browse.SortField(field), Desc: desc}
	sum, err := s.Summarize(id, spec, ranges)
	if err != nil {
		return nil, err
	}
	if sum.Count > browse.MaxLocate {
		return nil, fmt.Errorf("the selection has %d items, more than the %d kept across a reload", sum.Count, browse.MaxLocate)
	}
	return s.Keys(id, spec, version, ranges)
}

// ListingLocate returns the index of each key in the given order of a listing,
// or -1 for a key it does not hold.
func (a *App) ListingLocate(id string, field string, desc bool, keys []string) ([]int, error) {
	s, err := a.browser()
	if err != nil {
		return nil, err
	}
	return s.Locate(id, browse.SortSpec{Field: browse.SortField(field), Desc: desc}, keys)
}
