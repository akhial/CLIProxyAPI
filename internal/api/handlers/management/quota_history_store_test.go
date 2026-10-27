package management

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQuotaHistoryStore_AppendAndReadAcrossDays(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "quota-history")
	store := newQuotaHistoryStore(dir)
	day1 := time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	for _, sample := range []quotaHistorySample{
		{T: day1.UnixMilli(), AuthID: "a", Provider: "claude", Src: "poll", Windows: []quotaHistoryWindow{{ID: "five_hour", PeriodS: 18000, Used: 10}}},
		{T: day2.UnixMilli(), AuthID: "a", Provider: "claude", Src: "header", Windows: []quotaHistoryWindow{{ID: "five_hour", PeriodS: 18000, Used: 11, ResetAt: 1761900000000}}},
	} {
		if errAppend := store.Append(sample); errAppend != nil {
			t.Fatalf("Append: %v", errAppend)
		}
	}
	for _, name := range []string{"2026-10-25.jsonl", "2026-10-26.jsonl"} {
		info, errStat := os.Stat(filepath.Join(dir, name))
		if errStat != nil {
			t.Fatalf("expected daily file %s: %v", name, errStat)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s permissions = %o, want 600", name, perm)
		}
	}

	samples, errRead := store.Read(day1.Add(-time.Hour), day2.Add(time.Hour))
	if errRead != nil {
		t.Fatalf("Read: %v", errRead)
	}
	if len(samples) != 2 || samples[0].Windows[0].Used != 10 || samples[1].Windows[0].ResetAt != 1761900000000 {
		t.Fatalf("samples = %+v", samples)
	}

	onlySecond, errRead := store.Read(day2, day2)
	if errRead != nil {
		t.Fatalf("Read: %v", errRead)
	}
	if len(onlySecond) != 1 || onlySecond[0].Src != "header" {
		t.Fatalf("range-filtered samples = %+v", onlySecond)
	}

	missing, errRead := newQuotaHistoryStore(filepath.Join(t.TempDir(), "absent")).Read(day1, day2)
	if errRead != nil || len(missing) != 0 {
		t.Fatalf("missing directory: samples=%+v err=%v", missing, errRead)
	}
}

func TestQuotaHistoryStore_ToleratesTruncatedLastLine(t *testing.T) {
	dir := t.TempDir()
	store := newQuotaHistoryStore(dir)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if errAppend := store.Append(quotaHistorySample{T: at.UnixMilli(), AuthID: "a"}); errAppend != nil {
		t.Fatalf("Append: %v", errAppend)
	}
	path := filepath.Join(dir, "2026-10-26.jsonl")
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if _, errWrite := file.WriteString(`{"t":1761480000000,"auth_id":"b","wind`); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	samples, errRead := store.Read(at.Add(-time.Hour), at.Add(time.Hour))
	if errRead != nil {
		t.Fatalf("Read with truncated line: %v", errRead)
	}
	if len(samples) != 1 || samples[0].AuthID != "a" {
		t.Fatalf("samples = %+v", samples)
	}

	// The next append starts a fresh line instead of extending the broken one.
	if errAppend := store.Append(quotaHistorySample{T: at.Add(time.Minute).UnixMilli(), AuthID: "c"}); errAppend != nil {
		t.Fatalf("Append after truncation: %v", errAppend)
	}
	samples, errRead = store.Read(at.Add(-time.Hour), at.Add(time.Hour))
	if errRead != nil {
		t.Fatalf("Read: %v", errRead)
	}
	if len(samples) != 2 || samples[1].AuthID != "c" {
		t.Fatalf("samples after recovery = %+v", samples)
	}
}

func TestQuotaHistoryStore_ConcurrentAppends(t *testing.T) {
	store := newQuotaHistoryStore(t.TempDir())
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				sample := quotaHistorySample{
					T:       at.Add(time.Duration(i) * time.Second).UnixMilli(),
					AuthID:  fmt.Sprintf("auth-%d", writer),
					Windows: []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: float64(i)}},
				}
				if errAppend := store.Append(sample); errAppend != nil {
					t.Errorf("Append: %v", errAppend)
				}
			}
		}(w)
	}
	wg.Wait()
	samples, errRead := store.Read(at, at.Add(time.Hour))
	if errRead != nil {
		t.Fatalf("Read: %v", errRead)
	}
	if len(samples) != writers*perWriter {
		t.Fatalf("read %d samples, want %d", len(samples), writers*perWriter)
	}
}

func TestQuotaHistoryStore_PruneRetention(t *testing.T) {
	dir := t.TempDir()
	store := newQuotaHistoryStore(dir)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	files := map[string]bool{
		"2026-09-20.jsonl": false, // ends 2026-09-21, before the 35-day cutoff 2026-09-22 12:00
		"2026-09-21.jsonl": false, // ends 2026-09-22 00:00, still before the cutoff
		"2026-09-22.jsonl": true,  // holds samples after the cutoff
		"2026-10-27.jsonl": true,
		"notes.txt":        true,
		"bogus.jsonl":      true,
	}
	for name := range files {
		if errWrite := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	if errPrune := store.Prune(now); errPrune != nil {
		t.Fatalf("Prune: %v", errPrune)
	}
	for name, keep := range files {
		_, errStat := os.Stat(filepath.Join(dir, name))
		if exists := errStat == nil; exists != keep {
			t.Errorf("%s exists=%v, want %v", name, exists, keep)
		}
	}
	if errPrune := newQuotaHistoryStore(filepath.Join(dir, "absent")).Prune(now); errPrune != nil {
		t.Fatalf("Prune of missing directory: %v", errPrune)
	}
}
