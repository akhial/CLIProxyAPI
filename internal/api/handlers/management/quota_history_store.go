package management

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	quotaHistoryFileSuffix = ".jsonl"
	quotaHistoryDayLayout  = "2006-01-02"
	quotaHistoryRetention  = 35 * 24 * time.Hour
)

// quotaHistorySample is one JSONL record of the quota history store.
type quotaHistorySample struct {
	T         int64                `json:"t"`
	AuthID    string               `json:"auth_id"`
	AuthIndex string               `json:"auth_index"`
	FileName  string               `json:"file_name"`
	Provider  string               `json:"provider"`
	Label     string               `json:"label"`
	Email     string               `json:"email"`
	Plan      string               `json:"plan"`
	Src       string               `json:"src"`
	Windows   []quotaHistoryWindow `json:"windows"`
}

// quotaHistoryStore keeps samples in append-only daily files named
// YYYY-MM-DD.jsonl (UTC). The directory is created on the first write.
type quotaHistoryStore struct {
	dir string
	mu  sync.Mutex
}

func newQuotaHistoryStore(dir string) *quotaHistoryStore {
	return &quotaHistoryStore{dir: dir}
}

func (s *quotaHistoryStore) dayPath(day time.Time) string {
	return filepath.Join(s.dir, day.UTC().Format(quotaHistoryDayLayout)+quotaHistoryFileSuffix)
}

// Append writes one sample as a single line to the file of its UTC day.
func (s *quotaHistoryStore) Append(sample quotaHistorySample) error {
	line, errMarshal := json.Marshal(sample)
	if errMarshal != nil {
		return fmt.Errorf("quota history: marshal sample: %w", errMarshal)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if errMkdir := os.MkdirAll(s.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("quota history: create directory: %w", errMkdir)
	}
	path := s.dayPath(time.UnixMilli(sample.T))
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("quota history: open %s: %w", filepath.Base(path), errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("quota history: close %s: %v", filepath.Base(path), errClose)
		}
	}()
	// An interrupted write can leave the last line without its newline; start a
	// fresh line so this record is not glued onto the broken one.
	if info, errStat := file.Stat(); errStat == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, errRead := file.ReadAt(last, info.Size()-1); errRead == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, errWrite := file.Write(line); errWrite != nil {
		return fmt.Errorf("quota history: write %s: %w", filepath.Base(path), errWrite)
	}
	return nil
}

// Read returns the samples with from <= t <= to, in file order. Lines that do
// not decode (for example a truncated last line) are skipped.
func (s *quotaHistoryStore) Read(from, to time.Time) ([]quotaHistorySample, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	if fromMs > toMs {
		return nil, nil
	}
	first := quotaHistoryUTCDay(from)
	last := quotaHistoryUTCDay(to)
	var samples []quotaHistorySample
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		daySamples, errRead := s.readFile(s.dayPath(day), fromMs, toMs)
		if errRead != nil {
			return nil, errRead
		}
		samples = append(samples, daySamples...)
	}
	return samples, nil
}

func (s *quotaHistoryStore) readFile(path string, fromMs, toMs int64) ([]quotaHistorySample, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("quota history: open %s: %w", filepath.Base(path), errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("quota history: close %s: %v", filepath.Base(path), errClose)
		}
	}()

	var samples []quotaHistorySample
	reader := bufio.NewReader(file)
	for {
		line, errLine := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var sample quotaHistorySample
			if errDecode := json.Unmarshal(trimmed, &sample); errDecode == nil &&
				sample.AuthID != "" && sample.T >= fromMs && sample.T <= toMs {
				samples = append(samples, sample)
			}
		}
		if errLine != nil {
			if errors.Is(errLine, io.EOF) {
				return samples, nil
			}
			return nil, fmt.Errorf("quota history: read %s: %w", filepath.Base(path), errLine)
		}
	}
}

// Prune deletes daily files whose samples are all older than the retention period.
func (s *quotaHistoryStore) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, errList := os.ReadDir(s.dir)
	if errList != nil {
		if errors.Is(errList, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("quota history: list directory: %w", errList)
	}
	cutoff := now.Add(-quotaHistoryRetention)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, quotaHistoryFileSuffix) {
			continue
		}
		day, errParse := time.ParseInLocation(quotaHistoryDayLayout, strings.TrimSuffix(name, quotaHistoryFileSuffix), time.UTC)
		if errParse != nil {
			continue
		}
		if day.AddDate(0, 0, 1).After(cutoff) {
			continue
		}
		if errRemove := os.Remove(filepath.Join(s.dir, name)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warnf("quota history: failed to remove expired file %s", name)
		}
	}
	return nil
}

func quotaHistoryUTCDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
