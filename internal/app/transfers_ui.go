package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"s4browser/internal/browse"
	"s4browser/internal/queue"
)

// The bindings the transfer UI uses. QueueUploads and QueueDownloads return one
// item per path or key; these return a SubmitReport instead, whose size is
// bounded however much was requested, and a download takes the listing's
// selection (index ranges) so its keys never travel through the page.

// DownloadSelection asks for the rows selected in a listing to be downloaded
// into a local folder. The ranges are inclusive index ranges in the given sort
// order of the given listing version, as the listing table holds them.
type DownloadSelection struct {
	ProfileID string   `json:"profileId"`
	Bucket    string   `json:"bucket"`
	Region    string   `json:"region"`
	ListingID string   `json:"listingId"`
	Field     string   `json:"field"`
	Desc      bool     `json:"desc"`
	Version   uint64   `json:"version"`
	Ranges    [][2]int `json:"ranges"`
	DestDir   string   `json:"destDir"`
}

// StartUploads queues uploads of local files and folders, as QueueUploads, and
// reports the outcome in bounded form.
func (a *App) StartUploads(req queue.UploadRequest) (queue.SubmitReport, error) {
	res, err := a.QueueUploads(req)
	if err != nil && len(res.Items) == 0 {
		return queue.SubmitReport{}, err
	}
	return reportWith(res, err), nil
}

// StartDownloads resolves a listing selection to keys in Go and queues their
// downloads. Folders in the selection download with everything below them.
func (a *App) StartDownloads(sel DownloadSelection) (queue.SubmitReport, error) {
	b, err := a.browser()
	if err != nil {
		return queue.SubmitReport{}, err
	}
	keys, err := b.Keys(sel.ListingID, browse.SortSpec{Field: browse.SortField(sel.Field), Desc: sel.Desc}, sel.Version, sel.Ranges)
	if err != nil {
		if errors.Is(err, browse.ErrListingNotFound) {
			return queue.SubmitReport{}, errors.New("the folder was closed; open it and select the items again")
		}
		return queue.SubmitReport{}, err
	}
	if len(keys) == 0 {
		return queue.SubmitReport{}, errors.New("nothing is selected")
	}
	res, err := a.QueueDownloads(queue.DownloadRequest{
		ProfileID: sel.ProfileID, Bucket: sel.Bucket, Region: sel.Region, Keys: keys, DestDir: sel.DestDir,
	})
	if err != nil && len(res.Items) == 0 {
		return queue.SubmitReport{}, err
	}
	return reportWith(res, err), nil
}

// reportWith condenses a result. An error that ended the request after some
// items were handled (the engine closing) is reported as one more refusal, so
// the jobs already queued are not hidden behind an error.
func reportWith(res queue.SubmitResult, err error) queue.SubmitReport {
	rep := res.Report()
	if err != nil {
		rep.RefusedTotal++
		rep.Refused = append(rep.Refused, queue.Refusal{Code: queue.CodeStopped, Reason: err.Error()})
	}
	return rep
}

// Native pickers. Every one returns absolute paths, or nothing when the user
// cancels.

type pickKind int

const (
	pickFiles pickKind = iota
	pickFolder
	pickDest
)

// pick opens a native dialog. A devfake build can answer from the environment
// instead, so the UI can be driven without a person at the dialog.
func (a *App) pick(kind pickKind, start string) ([]string, error) {
	if paths, ok := devPick(kind); ok {
		return paths, nil
	}
	if a.wapp == nil {
		return nil, errors.New("the window is not ready")
	}
	d := a.wapp.Dialog.OpenFile().AttachToWindow(a.window)
	switch kind {
	case pickFiles:
		d.SetTitle("Upload files").CanChooseFiles(true).CanChooseDirectories(false).SetDirectory(existingDir(start))
		paths, err := d.PromptForMultipleSelection()
		if paths == nil {
			paths = []string{}
		}
		return paths, err
	case pickFolder:
		d.SetTitle("Upload a folder").CanChooseFiles(false).CanChooseDirectories(true).SetDirectory(existingDir(start))
		dir, err := d.PromptForSingleSelection()
		return nonEmpty(dir), err
	default:
		d.SetTitle("Download to folder").CanChooseFiles(false).CanChooseDirectories(true).
			CanCreateDirectories(true).SetDirectory(downloadStart(start))
		dir, err := d.PromptForSingleSelection()
		return nonEmpty(dir), err
	}
}

func nonEmpty(s string) []string {
	if s == "" {
		return []string{}
	}
	return []string{s}
}

// existingDir returns dir if it is an existing folder, or "" so the dialog
// opens where Windows chooses.
func existingDir(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// downloadStart is the folder the download picker opens in: the last one used,
// or the user's Downloads folder.
func downloadStart(last string) string {
	if d := existingDir(last); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return existingDir(filepath.Join(home, "Downloads"))
	}
	return ""
}

// ChooseUploadFiles opens the native file picker. It returns no paths when the
// user cancels.
func (a *App) ChooseUploadFiles(start string) ([]string, error) { return a.pick(pickFiles, start) }

// ChooseUploadFolder opens the native folder picker for an upload.
func (a *App) ChooseUploadFolder(start string) ([]string, error) { return a.pick(pickFolder, start) }

// ChooseDownloadFolder opens the native folder picker for a download
// destination, starting in the last folder used or in Downloads.
func (a *App) ChooseDownloadFolder(last string) ([]string, error) { return a.pick(pickDest, last) }

// beforeClose asks before quitting while transfers are unfinished, because
// quitting cancels them and nothing resumes after a restart. It returns true to
// keep the window open.
func (a *App) beforeClose(ctx context.Context) bool {
	if a.queue == nil {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := a.queue.Unfinished(qctx)
	if err != nil || n == 0 {
		return false
	}
	if a.wapp == nil {
		return false
	}
	answer := make(chan bool, 1)
	dlg := a.wapp.Dialog.Question().SetTitle("Quit s4browser?").
		SetMessage(fmt.Sprintf("%s not finished. Quitting cancels %s: unfinished uploads are aborted on S3 and "+
			"partial downloads are deleted. Nothing resumes after a restart.\n\nQuit anyway?",
			countPhrase(n), them(n))).
		AttachToWindow(a.window)
	yes := dlg.AddButton("Yes")
	yes.OnClick(func() { answer <- true })
	no := dlg.AddButton("No")
	no.OnClick(func() { answer <- false })
	dlg.SetDefaultButton(no).SetCancelButton(no)
	dlg.Show()
	select {
	case yes := <-answer:
		return !yes
	case <-ctx.Done():
		return true
	}
}

func countPhrase(n int) string {
	if n == 1 {
		return "1 transfer is"
	}
	return fmt.Sprintf("%d transfers are", n)
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
