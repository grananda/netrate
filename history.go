package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

const (
	// historyLimit is how many completed measurements the history keeps.
	historyLimit = 10

	// historyVersion is the layout version written to the file. It is written
	// but not checked on read: version 1 is the only one that exists, and a
	// check belongs to the change that introduces version 2.
	historyVersion = 1

	// historyDirName is the directory under the user config directory that
	// holds everything netrate stores.
	historyDirName = "netrate"

	// historyFileName is the history file inside historyDirName.
	historyFileName = "history.json"
)

// errNoHistoryPath is returned by a store that was never given a location. The
// zero historyStore therefore does no IO, so a model built without one can
// never touch the real config directory.
var errNoHistoryPath = errors.New("history location unknown")

// errNoConfigDir is wrapped by the store when the user's configuration
// directory cannot be determined.
var errNoConfigDir = errors.New("no config directory")

// runRecord is one completed measurement as it is stored. The JSON keys are the
// file's contract.
type runRecord struct {
	At              time.Time `json:"at"`
	URL             string    `json:"url"`
	MedianMbps      float64   `json:"median_mbps"`
	TrimmedMbps     float64   `json:"trimmed_mbps"`
	MinMbps         float64   `json:"min_mbps"`
	MaxMbps         float64   `json:"max_mbps"`
	CVPercent       float64   `json:"cv_percent"`
	Stable          bool      `json:"stable"`
	EndedEarly      bool      `json:"ended_early"`
	Bytes           int64     `json:"bytes"`
	DurationSeconds float64   `json:"duration_seconds"`
	Samples         int       `json:"samples"`
	Streams         int       `json:"streams"`
}

// historyDoc is the whole history file.
type historyDoc struct {
	Version      int         `json:"version"`
	Measurements []runRecord `json:"measurements"`
}

// newRunRecord turns a verdict into a stored record. The timestamp is cut to
// whole seconds, which is plenty for ten records, and credentials are removed
// from the URL because the file is a plain-text copy of what was measured.
func newRunRecord(r Result, rawURL string, at time.Time) runRecord {
	return runRecord{
		At:              at.Truncate(time.Second),
		URL:             stripCredentials(rawURL),
		MedianMbps:      r.Median,
		TrimmedMbps:     r.Trimmed,
		MinMbps:         r.Min,
		MaxMbps:         r.Max,
		CVPercent:       r.CV,
		Stable:          r.Stable,
		EndedEarly:      r.EndedEarly,
		Bytes:           r.Bytes,
		DurationSeconds: r.Duration.Seconds(),
		Samples:         r.Samples,
		Streams:         r.Streams,
	}
}

// stripCredentials removes user[:password]@ from a URL. A URL that cannot be
// parsed yields "" rather than a string that might still carry a secret.
func stripCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// historyStore is where the history lives. It is a value so tests can point it
// at a temporary directory; err carries a failure to find a location at all.
type historyStore struct {
	path string
	err  error
}

// defaultHistoryStore resolves the per-user location once, at start-up.
func defaultHistoryStore() historyStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return historyStore{err: fmt.Errorf("%w: %v", errNoConfigDir, err)}
	}
	return historyStore{path: filepath.Join(dir, historyDirName, historyFileName)}
}

// location reports why the store cannot be used, if it cannot.
func (s historyStore) location() error {
	if s.err != nil {
		return s.err
	}
	if s.path == "" {
		return errNoHistoryPath
	}
	return nil
}

// load returns the stored records, oldest first, at most historyLimit of them.
// A missing file is an empty history, not an error. Sorting by time means a
// hand-edited file still reads in order.
func (s historyStore) load() ([]runRecord, error) {
	if err := s.location(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history file could not be read: %w", err)
	}
	var doc historyDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("history file could not be read: %w", err)
	}
	slices.SortStableFunc(doc.Measurements, func(a, b runRecord) int { return a.At.Compare(b.At) })
	return lastRecords(doc.Measurements), nil
}

// save appends rec and writes the newest historyLimit records back.
//
// The file is re-read first, so a run started earlier by another instance is
// not overwritten by this one's stale copy. There is no locking: a record is
// lost only if two runs finish in the same instant. A file that cannot be read
// is replaced, since keeping it would block every future save. The new record is
// appended rather than re-sorted in, so it is always kept and listed newest,
// whatever dates a hand-edited file holds.
func (s historyStore) save(rec runRecord) ([]runRecord, error) {
	if err := s.location(); err != nil {
		return nil, err
	}
	existing, err := s.load()
	if err != nil {
		existing = nil
	}
	runs := lastRecords(append(existing, rec))

	data, err := json.MarshalIndent(historyDoc{Version: historyVersion, Measurements: runs}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	return runs, nil
}

// lastRecords keeps the newest historyLimit entries of a list ordered oldest
// first.
func lastRecords(recs []runRecord) []runRecord {
	if len(recs) > historyLimit {
		recs = recs[len(recs)-historyLimit:]
	}
	return recs
}

// writeFileAtomic writes to a temporary file in the same directory and renames
// it over the target. A crash or a full disk mid-write therefore leaves the
// previous file intact instead of a truncated one. CreateTemp makes the file
// 0600, which the history wants: it names the URL that was measured.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "history-*.json.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// saveFailureReason is the short form of a save error for the one-line
// warning: the operating system's reason without the path, which can be long
// and would not fit a narrow box.
func saveFailureReason(err error) string {
	if errors.Is(err, errNoConfigDir) {
		return errNoConfigDir.Error()
	}
	// The usual causes get phrases short enough to fit the narrowest box
	// after the warning prefix; the operating system's own wording does not.
	switch {
	case errors.Is(err, syscall.EROFS):
		return "read-only disk"
	case errors.Is(err, syscall.ENOSPC):
		return "disk full"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "missing directory"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}
