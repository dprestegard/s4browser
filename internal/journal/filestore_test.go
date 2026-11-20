package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const lockHolderEnv = "S4BROWSER_JOURNAL_LOCK_HOLDER"

// TestMain lets this test binary act as a second app instance holding the lock.
func TestMain(m *testing.M) {
	if dir := os.Getenv(lockHolderEnv); dir != "" {
		store, err := OpenFile(dir)
		if err != nil {
			fmt.Println("open failed:", err)
			os.Exit(3)
		}
		defer store.Close()
		fmt.Println("holding")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func openStore(t *testing.T, dir string) *FileStore {
	t.Helper()
	store, err := OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestFileStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, uploadEntry()); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, tempEntry()); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, tempEntry().ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, uploadEntry()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close: got %v", err)
	}

	reopened := openStore(t, dir)
	if got, want := reopened.List(), []Entry{uploadEntry()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, TempFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestFileStoreRejectsInvalidEntry(t *testing.T) {
	store := openStore(t, t.TempDir())
	if err := store.Put(context.Background(), Entry{ID: "x"}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("got %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatal("an invalid entry was stored")
	}
}

func TestFileStoreConcurrentPuts(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := uploadEntry()
			entry.ID = fmt.Sprintf("job-%02d", i)
			if err := store.Put(context.Background(), entry); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Decode(data)
	if err != nil || len(entries) != 32 {
		t.Fatalf("%d entries on disk, err %v", len(entries), err)
	}
}

func TestFileStoreHonoursCancelledContext(t *testing.T) {
	store := openStore(t, t.TempDir())
	store.write <- struct{}{} // Another write is in progress.
	defer func() { <-store.write }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Put(ctx, uploadEntry()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRemovesStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, TempFileName)
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale snapshot still present: %v", err)
	}
}

func TestOpenSetsAsideCorruptJournal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":1,"entries":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, dir)
	if len(store.List()) != 0 || len(store.Warnings()) != 1 {
		t.Fatalf("entries %v, warnings %v", store.List(), store.Warnings())
	}
	moved, err := filepath.Glob(filepath.Join(dir, CorruptPrefix+"*.json"))
	if err != nil || len(moved) != 1 {
		t.Fatalf("corrupt file not set aside: %v %v", moved, err)
	}
}

func TestOpenLeavesNewerJournalAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// The newer format has a field this build does not know (review F2).
	content := []byte(`{"version":2,"entries":[{"id":"a-1","kind":"download-temp","phase":"active","createdAt":"2026-11-17T12:00:00Z","tempPath":"x","checksum":"y"}]}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(dir); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("file changed: %q, %v", got, err)
	}
	// The failed open must have released the lock.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir)
}

func TestSecondOpenInSameProcessIsLocked(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	if _, err := OpenFile(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir)
}

// TestSecondProcessCannotTakeOverJournal uses a real second process, then kills
// it to prove that a crashed instance does not leave the journal locked.
func TestSecondProcessCannotTakeOverJournal(t *testing.T) {
	dir := t.TempDir()
	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), lockHolderEnv+"="+dir)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "holding" {
		t.Fatalf("holder said %q, %v", line, err)
	}

	if _, err := OpenFile(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second instance: got %v, want ErrLocked", err)
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		store, err := OpenFile(dir)
		if err == nil {
			store.Close()
			return
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("after the holder died: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
