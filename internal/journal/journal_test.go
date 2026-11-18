package journal

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func uploadEntry() Entry {
	return Entry{
		ID: "0123456789abcdef-1", Kind: KindMultipartUpload, Phase: PhaseActive, CreatedAt: testTime,
		Profile: "work", Region: "us-east-1", Bucket: "media", Key: "show/episode.mkv", UploadID: "upload-1",
	}
}

func tempEntry() Entry {
	return Entry{
		ID: "fedcba9876543210-1", Kind: KindDownloadTemp, Phase: PhaseActive, CreatedAt: testTime,
		TempPath: `F:\media\~s4-fedcba9876543210.s4tmp`,
	}
}

// TestEntryFieldAllowlist forces a secrecy review whenever Entry gains a field.
func TestEntryFieldAllowlist(t *testing.T) {
	allowed := []string{
		"ID", "Kind", "Phase", "CreatedAt", "Profile", "Region", "Bucket", "Key", "UploadID", "TempPath",
	}
	entryType := reflect.TypeFor[Entry]()
	var got []string
	for field := range entryType.Fields() {
		got = append(got, field.Name)
	}
	if !reflect.DeepEqual(got, allowed) {
		t.Fatalf("Entry fields changed: got %v. Review for secrets, then update this list", got)
	}
}

func TestEncodedJournalHasNoSecretShapedContent(t *testing.T) {
	data, err := Encode([]Entry{uploadEntry(), tempEntry()})
	if err != nil {
		t.Fatal(err)
	}
	secretShapes := regexp.MustCompile(`(?i)secret|session.?token|password|authorization|signature|AKIA|ASIA`)
	if match := secretShapes.Find(data); match != nil {
		t.Fatalf("journal contains %q:\n%s", match, data)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	entries := []Entry{tempEntry(), uploadEntry()}
	data, err := Encode(entries)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{uploadEntry(), tempEntry()} // Sorted by ID.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestEncodeEmptyJournal(t *testing.T) {
	data, err := Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"entries": []`) {
		t.Fatalf("got %s", data)
	}
	if entries, err := Decode(data); err != nil || len(entries) != 0 {
		t.Fatalf("got %v, %v", entries, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid := `{"id":"a-1","kind":"download-temp","phase":"active","createdAt":"2026-11-17T12:00:00Z","tempPath":"x"}`
	tests := []struct {
		name string
		data string
		want error
	}{
		{"truncated", `{"version":1,"entries":[`, ErrCorrupt},
		{"empty", ``, ErrCorrupt},
		{"trailing data", `{"version":1,"entries":[]} {}`, ErrCorrupt},
		{"unknown top field", `{"version":1,"entries":[],"extra":1}`, ErrCorrupt},
		{"smuggled secret", `{"version":1,"entries":[{"id":"a-1","secretAccessKey":"x"}]}`, ErrCorrupt},
		{"invalid entry", `{"version":1,"entries":[{"id":"a-1"}]}`, ErrCorrupt},
		{"duplicate", `{"version":1,"entries":[` + valid + `,` + valid + `]}`, ErrCorrupt},
		{"future version", `{"version":2,"entries":[]}`, ErrUnsupportedVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode([]byte(tt.data)); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEntryValidate(t *testing.T) {
	creating := uploadEntry()
	creating.Phase = PhaseCreating
	creating.UploadID = ""
	if err := creating.Validate(); err != nil {
		t.Fatalf("creating entry: %v", err)
	}

	tests := map[string]func(*Entry){
		"no ID":                func(e *Entry) { e.ID = "" },
		"no time":              func(e *Entry) { e.CreatedAt = time.Time{} },
		"unknown kind":         func(e *Entry) { e.Kind = "other" },
		"unknown phase":        func(e *Entry) { e.Phase = "other" },
		"no bucket":            func(e *Entry) { e.Bucket = "" },
		"active without ID":    func(e *Entry) { e.UploadID = "" },
		"creating with ID":     func(e *Entry) { e.Phase = PhaseCreating },
		"upload with tempPath": func(e *Entry) { e.TempPath = "x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			entry := uploadEntry()
			mutate(&entry)
			if err := entry.Validate(); !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("got %v", err)
			}
		})
	}

	badTemp := tempEntry()
	badTemp.Bucket = "media"
	if err := badTemp.Validate(); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("temp with bucket: got %v", err)
	}
}

func TestMatchesIntent(t *testing.T) {
	intent := uploadEntry()
	intent.Phase = PhaseCreating
	intent.UploadID = ""

	tests := []struct {
		name      string
		key       string
		initiated time.Time
		want      bool
	}{
		{"same instant", intent.Key, testTime, true},
		{"earliest edge", intent.Key, testTime.Add(-IntentSkewBefore), true},
		{"latest edge", intent.Key, testTime.Add(IntentWindow), true},
		{"too early", intent.Key, testTime.Add(-IntentSkewBefore - time.Second), false},
		{"too late", intent.Key, testTime.Add(IntentWindow + time.Second), false},
		{"longer key sharing the prefix", intent.Key + ".bak", testTime, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesIntent(intent, RemoteUpload{Key: tt.key, UploadID: "u", Initiated: tt.initiated})
			if got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}

	if MatchesIntent(uploadEntry(), RemoteUpload{Key: intent.Key, Initiated: testTime}) {
		t.Fatal("active entries are aborted by ID and must never match by key")
	}
}
