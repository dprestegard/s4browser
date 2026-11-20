package queue

import (
	"fmt"
	"testing"
)

func TestReportCondensesResult(t *testing.T) {
	big := SubmitItem{Source: `C:\big`, Kind: KindFolder, Queued: 5000, Examined: 5300, EmptyFolders: 3, RefusedTotal: 300}
	for i := range MaxListedRefusals {
		big.Refused = append(big.Refused, Refusal{Path: fmt.Sprintf(`C:\big\f%d`, i), Code: "reserved-name", Reason: "r"})
	}
	res := SubmitResult{Items: []SubmitItem{
		{Source: `C:\a.bin`, Kind: KindFile, JobID: "j1", Queued: 1, Examined: 1},
		{Source: `C:\gone.bin`, Kind: KindFile, Error: "not found", Code: CodeNotFound, Examined: 1},
		big,
		{Source: "p/", Kind: KindPrefix, Queued: 2, Placeholders: 4, Error: "stopped after 2", Code: CodeStopped},
	}}
	rep := res.Report()
	if rep.Requested != 4 || rep.Queued != 5003 || rep.EmptyFolders != 3 || rep.Placeholders != 4 || rep.Examined != 5302 {
		t.Fatalf("counts = %+v", rep)
	}
	if rep.RefusedTotal != res.Refused() || rep.RefusedTotal != 302 {
		t.Fatalf("refusedTotal = %d, Refused() = %d, want 302", rep.RefusedTotal, res.Refused())
	}
	if len(rep.Refused) != MaxListedRefusals {
		t.Fatalf("listed %d refusals, want the cap %d", len(rep.Refused), MaxListedRefusals)
	}
	if f := rep.Refused[0]; f.Path != `C:\gone.bin` || f.Code != CodeNotFound || f.Reason != "not found" {
		t.Fatalf("a whole-item failure is listed under its own name first, got %+v", f)
	}
	if empty := (SubmitResult{}).Report(); empty.Refused == nil {
		t.Fatal("Refused must be an empty list, not null, for the UI")
	}
}
