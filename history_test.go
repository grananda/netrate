package main

import (
	"encoding/json"
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tempStore is a history store inside a fresh temporary directory. No test
// touches the real user config directory.
func tempStore(t *testing.T) historyStore {
	t.Helper()
	return historyStore{path: filepath.Join(t.TempDir(), "netrate", "history.json")}
}

// blockedStore points at a path whose parent is a regular file, so neither the
// directory nor the file can ever be created.
func blockedStore(t *testing.T) historyStore {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return historyStore{path: filepath.Join(blocker, "netrate", "history.json")}
}

// testRecord builds a record with a recognisable median.
func testRecord(at time.Time, median float64) runRecord {
	return runRecord{
		At: at.Truncate(time.Second), URL: "https://host.example/f", MedianMbps: median,
		TrimmedMbps: median, MinMbps: median / 2, MaxMbps: median * 2, CVPercent: 1.5,
		Stable: true, Bytes: 1000, DurationSeconds: 5.5, Samples: 30, Streams: 4,
	}
}

// seedRecords writes records to the store's file exactly as given, in that
// order, bypassing save so tests control ordering and count.
func seedRecords(t *testing.T, s historyStore, recs ...runRecord) {
	t.Helper()
	doc := historyDoc{Version: 1, Measurements: recs}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	seedRaw(t, s, string(data))
}

func seedRaw(t *testing.T, s historyStore, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readDoc decodes the history file as the contract describes it.
func readDoc(t *testing.T, path string) historyDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading history file: %v", err)
	}
	var doc historyDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("history file is not valid JSON: %v\n%s", err, data)
	}
	return doc
}

func medians(recs []runRecord) []float64 {
	var out []float64
	for _, r := range recs {
		out = append(out, r.MedianMbps)
	}
	return out
}

// AC-7
func TestHistoryStoreUsesUserConfigDir(t *testing.T) {
	d := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", d)
	case "darwin":
		t.Setenv("HOME", d)
	case "plan9":
		t.Skip("no user config directory convention to point at")
	default:
		t.Setenv("XDG_CONFIG_HOME", d)
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}

	s := defaultHistoryStore()
	want := filepath.Join(cfgDir, "netrate", "history.json")
	if s.path != want {
		t.Fatalf("path = %q, want %q", s.path, want)
	}
	if !strings.HasPrefix(s.path, d) {
		t.Fatalf("path %q is outside the temporary config dir %q", s.path, d)
	}

	if _, err := s.save(testRecord(time.Now(), 100)); err != nil {
		t.Fatalf("save: %v", err)
	}
	doc := readDoc(t, want)
	if doc.Version != 1 || len(doc.Measurements) != 1 {
		t.Fatalf("doc = %+v, want version 1 with one record", doc)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(want))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("dir mode = %o, want 700", got)
		}
		fileInfo, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("file mode = %o, want 600", got)
		}
	}
}

// AC-15
func TestHistoryStoreWithoutConfigDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the config dir can only be made unknown this way on Linux")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	s := defaultHistoryStore()
	if s.err == nil {
		t.Fatal("defaultHistoryStore().err is nil with no config directory")
	}
	if _, err := s.save(testRecord(time.Now(), 1)); err == nil {
		t.Error("save succeeded without a config directory")
	}
	if _, err := s.load(); err == nil {
		t.Error("load succeeded without a config directory")
	}

	t.Run("the zero store does no IO", func(t *testing.T) {
		cwd := t.TempDir()
		t.Chdir(cwd)
		var zero historyStore
		if _, err := zero.save(testRecord(time.Now(), 1)); err != errNoHistoryPath {
			t.Errorf("save error = %v, want errNoHistoryPath", err)
		}
		if _, err := zero.load(); err != errNoHistoryPath {
			t.Errorf("load error = %v, want errNoHistoryPath", err)
		}
		entries, _ := os.ReadDir(cwd)
		if len(entries) != 0 {
			t.Errorf("the zero store created %d entries", len(entries))
		}
	})
}

// AC-2, AC-5, AC-6
func TestHistoryStoreKeepsTheTenMostRecent(t *testing.T) {
	base := time.Now().Add(-100 * 24 * time.Hour)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }

	t.Run("fewer than ten grows by one", func(t *testing.T) {
		s := tempStore(t)
		seedRecords(t, s, testRecord(at(1), 1), testRecord(at(2), 2), testRecord(at(3), 3))
		got, err := s.save(testRecord(at(50), 99))
		if err != nil {
			t.Fatal(err)
		}
		if want := []float64{1, 2, 3, 99}; !slices.Equal(medians(got), want) {
			t.Fatalf("returned %v, want %v", medians(got), want)
		}
		if m := medians(readDoc(t, s.path).Measurements); !slices.Equal(m, []float64{1, 2, 3, 99}) {
			t.Fatalf("file has %v", m)
		}
	})

	t.Run("ten drops the oldest", func(t *testing.T) {
		s := tempStore(t)
		var recs []runRecord
		for i := 1; i <= 10; i++ {
			recs = append(recs, testRecord(at(i), float64(i)))
		}
		seedRecords(t, s, recs...)
		if _, err := s.save(testRecord(at(50), 99)); err != nil {
			t.Fatal(err)
		}
		want := []float64{2, 3, 4, 5, 6, 7, 8, 9, 10, 99}
		if m := medians(readDoc(t, s.path).Measurements); !slices.Equal(m, want) {
			t.Fatalf("file has %v, want %v", m, want)
		}
	})

	t.Run("twelve out of order end as exactly ten with the new one last", func(t *testing.T) {
		s := tempStore(t)
		var recs []runRecord
		for _, i := range []int{7, 1, 12, 3, 9, 2, 11, 5, 10, 4, 8, 6} {
			recs = append(recs, testRecord(at(i), float64(i)))
		}
		seedRecords(t, s, recs...)
		if _, err := s.save(testRecord(at(50), 99)); err != nil {
			t.Fatal(err)
		}
		m := medians(readDoc(t, s.path).Measurements)
		if len(m) != 10 {
			t.Fatalf("file has %d records, want 10: %v", len(m), m)
		}
		if m[len(m)-1] != 99 {
			t.Fatalf("new record is not last: %v", m)
		}
	})

	t.Run("a hand-edited future date does not push the new record out", func(t *testing.T) {
		s := tempStore(t)
		var recs []runRecord
		for i := 1; i <= 10; i++ {
			recs = append(recs, testRecord(time.Now().Add(time.Duration(i)*24*time.Hour), float64(i)))
		}
		seedRecords(t, s, recs...)
		if _, err := s.save(testRecord(time.Now(), 99)); err != nil {
			t.Fatal(err)
		}
		m := medians(readDoc(t, s.path).Measurements)
		if len(m) != 10 || m[len(m)-1] != 99 {
			t.Fatalf("file has %v, want 10 records ending with the new one", m)
		}
	})
}

// AC-6, AC-9, AC-14
func TestHistoryStoreLoad(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		recs, err := tempStore(t).load()
		if err != nil || len(recs) != 0 {
			t.Fatalf("got %v, %v; want no records and no error", recs, err)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		s := tempStore(t)
		seedRaw(t, s, `{"version":1,"measurements":[]}`)
		recs, err := s.load()
		if err != nil || len(recs) != 0 {
			t.Fatalf("got %v, %v; want no records and no error", recs, err)
		}
	})

	t.Run("twelve shuffled records give the ten newest, oldest first", func(t *testing.T) {
		s := tempStore(t)
		base := time.Now().Add(-48 * time.Hour)
		var recs []runRecord
		for _, i := range []int{5, 12, 1, 8, 3, 10, 7, 2, 11, 4, 9, 6} {
			recs = append(recs, testRecord(base.Add(time.Duration(i)*time.Minute), float64(i)))
		}
		seedRecords(t, s, recs...)
		got, err := s.load()
		if err != nil {
			t.Fatal(err)
		}
		want := []float64{3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
		if !slices.Equal(medians(got), want) {
			t.Fatalf("got %v, want %v", medians(got), want)
		}
	})

	for _, content := range []string{"not json", "[]"} {
		t.Run("unreadable: "+content, func(t *testing.T) {
			s := tempStore(t)
			seedRaw(t, s, content)
			recs, err := s.load()
			if err == nil {
				t.Fatal("load returned no error")
			}
			if len(recs) != 0 {
				t.Fatalf("load returned records %v alongside the error", recs)
			}
		})
	}
}

// AC-14
func TestHistoryStoreReplacesACorruptFile(t *testing.T) {
	s := tempStore(t)
	seedRaw(t, s, "{oops")

	if _, err := s.save(testRecord(time.Now(), 42)); err != nil {
		t.Fatalf("save over a corrupt file failed: %v", err)
	}
	doc := readDoc(t, s.path)
	if len(doc.Measurements) != 1 || doc.Measurements[0].MedianMbps != 42 {
		t.Fatalf("file = %+v, want exactly the new record", doc)
	}
}

// AC-16
func TestHistoryStoreWriteIsAtomic(t *testing.T) {
	t.Run("no temporary file is left behind", func(t *testing.T) {
		s := tempStore(t)
		if _, err := s.save(testRecord(time.Now(), 1)); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Dir(s.path))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "history.json" {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("directory holds %v, want only history.json", names)
		}
	})

	t.Run("a leftover half-written temp file changes nothing", func(t *testing.T) {
		s := tempStore(t)
		seedRecords(t, s, testRecord(time.Now().Add(-time.Hour), 7))
		stray := filepath.Join(filepath.Dir(s.path), "history-123.json.tmp")
		if err := os.WriteFile(stray, []byte(`{"version":1,"measurem`), 0o600); err != nil {
			t.Fatal(err)
		}
		recs, err := s.load()
		if err != nil || !slices.Equal(medians(recs), []float64{7}) {
			t.Fatalf("load = %v, %v; want the intact previous history", recs, err)
		}
		if _, err := s.save(testRecord(time.Now(), 8)); err != nil {
			t.Fatalf("save next to a stray temp file failed: %v", err)
		}
		if m := medians(readDoc(t, s.path).Measurements); !slices.Equal(m, []float64{7, 8}) {
			t.Fatalf("file has %v", m)
		}
	})

	t.Run("a failed write leaves the previous file intact", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory permissions do not block writes on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		s := tempStore(t)
		seedRecords(t, s, testRecord(time.Now().Add(-time.Hour), 7))
		before, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(s.path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		if _, err := s.save(testRecord(time.Now(), 8)); err == nil {
			t.Fatal("save into a read-only directory succeeded")
		}
		after, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatalf("file changed after a failed save:\nbefore %s\nafter  %s", before, after)
		}
	})
}

// AC-15
func TestHistoryStoreUnwritable(t *testing.T) {
	s := blockedStore(t)
	_, err := s.save(testRecord(time.Now(), 1))
	if err == nil {
		t.Fatal("save under a regular file succeeded")
	}
	reason := saveFailureReason(err)
	if reason == "" {
		t.Fatal("saveFailureReason is empty")
	}
	if strings.Contains(reason, "/") {
		t.Fatalf("reason %q contains a path", reason)
	}
}

// AC-17
func TestNewRunRecord(t *testing.T) {
	r := Result{
		Median: 623.5, Trimmed: 623.6, Min: 478.1, Max: 876.2, CV: 0.2,
		Stable: true, EndedEarly: true, Bytes: 389337907,
		Duration: 5400 * time.Millisecond, Samples: 30, Streams: 6,
	}
	zone := time.FixedZone("test", 2*3600)
	at := time.Date(2026, 10, 19, 14, 3, 12, 987654321, zone)

	rec := newRunRecord(r, "https://user:pass@host.example/1G.iso", at)

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wantKeys := []string{"at", "bytes", "cv_percent", "duration_seconds", "ended_early", "max_mbps",
		"median_mbps", "min_mbps", "samples", "stable", "streams", "trimmed_mbps", "url"}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}

	checks := map[string]any{
		"median_mbps": 623.5, "trimmed_mbps": 623.6, "min_mbps": 478.1, "max_mbps": 876.2,
		"cv_percent": 0.2, "stable": true, "ended_early": true, "bytes": float64(389337907),
		"duration_seconds": r.Duration.Seconds(), "samples": float64(30), "streams": float64(6),
		"url": "https://host.example/1G.iso",
	}
	for k, want := range checks {
		if fields[k] != want {
			t.Errorf("%s = %v, want %v", k, fields[k], want)
		}
	}

	wantAt := at.Truncate(time.Second).Format(time.RFC3339)
	if fields["at"] != wantAt {
		t.Errorf("at = %v, want %s", fields["at"], wantAt)
	}
	if _, err := time.Parse(time.RFC3339, fields["at"].(string)); err != nil {
		t.Errorf("at does not parse as RFC 3339: %v", err)
	}

	t.Run("a user without a password is stripped too", func(t *testing.T) {
		got := newRunRecord(r, "https://user@host.example/f", at)
		if got.URL != "https://host.example/f" {
			t.Fatalf("url = %q", got.URL)
		}
	})
}

func TestStripCredentials(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"user and password", "https://user:secret@host.example/f.bin", "https://host.example/f.bin"},
		{"user only", "https://token@host.example/f", "https://host.example/f"},
		{"no credentials", "https://host.example/f?x=1", "https://host.example/f?x=1"},
		{"unparseable yields empty", "http://[::1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripCredentials(tt.in); got != tt.want {
				t.Errorf("stripCredentials(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSaveFailureReason(t *testing.T) {
	pe := &fs.PathError{Op: "open", Path: "/a/very/long/path/history.json", Err: syscall.EACCES}
	if got, want := saveFailureReason(fmt.Errorf("save history: %w", pe)), syscall.EACCES.Error(); got != want {
		t.Errorf("path error: got %q, want %q (no path)", got, want)
	}
	if got := saveFailureReason(errNoHistoryPath); got != errNoHistoryPath.Error() {
		t.Errorf("plain error: got %q", got)
	}
}

func TestSaveFailureReasonMapping(t *testing.T) {
	wrap := func(e error) error {
		return fmt.Errorf("save history: %w", &fs.PathError{Op: "open", Path: "/x/history.json", Err: e})
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"read-only", wrap(syscall.EROFS), "read-only disk"},
		{"disk full", wrap(syscall.ENOSPC), "disk full"},
		{"permission sentinel", wrap(fs.ErrPermission), "permission denied"},
		{"enoent", wrap(syscall.ENOENT), "missing directory"},
		{"not exist sentinel", wrap(fs.ErrNotExist), "missing directory"},
		{"no config dir", fmt.Errorf("save history: %w", errNoConfigDir), errNoConfigDir.Error()},
		{"other path error", wrap(syscall.EIO), syscall.EIO.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := saveFailureReason(tt.err); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHistoryRowsColumnDropping(t *testing.T) {
	runs := []runRecord{
		testRecord(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), 100),
		testRecord(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC), 200),
	}
	full := historyRows(runs, 80)
	if len(full) != 2 {
		t.Fatalf("rows = %d, want 2", len(full))
	}
	for _, want := range []string{"CV 1.5%", "5.5s"} {
		if !strings.Contains(full[0], want) {
			t.Errorf("wide row %q lacks %q", full[0], want)
		}
	}
	// Newest first.
	if !strings.Contains(full[0], "200") || !strings.Contains(full[1], "100") {
		t.Errorf("not newest first: %q / %q", full[0], full[1])
	}
	narrow := historyRows(runs, 1)
	for _, row := range narrow {
		if strings.Contains(row, "CV") || strings.Contains(row, "5.5s") {
			t.Errorf("narrow row kept optional columns: %q", row)
		}
		if !strings.Contains(row, "2026-01-0") {
			t.Errorf("narrow row lost the date: %q", row)
		}
	}
	// Middle width: CV fits, duration does not. 44 is one short of the text-only
	// row (45); at 79 the chart now just gets shorter.
	mid := historyRows(runs, 44)
	if !strings.Contains(mid[0], "CV") || strings.Contains(mid[0], "5.5s") {
		t.Errorf("mid row should drop only duration: %q", mid[0])
	}
}

// AC-19: the README documents the feature.
func TestReadmeDocumentsHistory(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"history.json", "h history", "$XDG_CONFIG_HOME", "Application Support", "%AppData%", "-plain", "last 10"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("README lacks %q", want)
		}
	}
}

func TestSaveFailureReasonLinkError(t *testing.T) {
	le := &os.LinkError{Op: "rename", Old: "/a/long/tmp-123", New: "/a/long/history.json", Err: syscall.EISDIR}
	if got, want := saveFailureReason(fmt.Errorf("save: %w", le)), syscall.EISDIR.Error(); got != want {
		t.Errorf("link error: got %q, want %q (no paths)", got, want)
	}
}

// barGlyphs are the block glyphs of a bar. The thin line of the range is left
// out because the box border also uses it.
const barGlyphs = "▓▒░▏▎▍▌▋▊▉█"

func hasBarGlyphs(row string) bool { return strings.ContainsAny(row, barGlyphs) }

func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = stripANSI(r)
	}
	return out
}

// AC-1, AC-4, AC-5, AC-8, AC-13, AC-16
func TestHistoryRowsChart(t *testing.T) {
	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)
	r1, r2, r3 := testRecord(d1, 100), testRecord(d2, 400), testRecord(d3, 50)
	r2.Stable = false
	const width, cells = 100, 53

	raw := historyRows([]runRecord{r1, r2, r3}, width)
	if len(raw) != 3 {
		t.Fatalf("rows = %d, want 3", len(raw))
	}
	for i, row := range raw {
		if w := lipgloss.Width(row); w != width {
			t.Errorf("row %d is %d cells, want %d", i, w, width)
		}
	}
	rows := plainRows(raw)

	// AC-8: newest first.
	for i, rec := range []runRecord{r3, r2, r1} {
		if want := rec.At.Local().Format(dateLayout); !strings.HasPrefix(rows[i], want) {
			t.Errorf("row %d = %q, want it to start with %s", i, rows[i], want)
		}
	}

	medianCol := -1
	for i, row := range rows {
		rs := []rune(row)
		if len(rs) != width {
			t.Fatalf("row %d has %d runes, want %d: %q", i, len(rs), width, row)
		}
		// AC-13: same chart area and gaps on every row.
		if got := string(rs[16:18]); got != "  " {
			t.Errorf("row %d: gap after the date is %q", i, got)
		}
		for _, r := range string(rs[18 : 18+cells]) {
			if !strings.ContainsRune(barGlyphs+"─ ", r) {
				t.Errorf("row %d: chart area holds %q: %q", i, r, string(rs[18:18+cells]))
				break
			}
		}
		if got := string(rs[18+cells : 18+cells+2]); got != "  " {
			t.Errorf("row %d: gap after the chart is %q", i, got)
		}
		col := strings.Index(string(rs[18+cells:]), "Mbps")
		if medianCol < 0 {
			medianCol = col
		} else if col != medianCol {
			t.Errorf("row %d: the median ends at column %d, others at %d", i, col, medianCol)
		}
	}

	// AC-1, AC-12: the fastest fills the chart with whole-cell shade glyphs.
	fastest := string([]rune(rows[1])[18 : 18+cells])
	if n := len([]rune(fastest)); n != cells {
		t.Errorf("fastest row chart has %d cells, want %d", n, cells)
	}
	for _, r := range fastest {
		if !strings.ContainsRune("▓▒░", r) {
			t.Errorf("fastest row chart holds %q (no partial block or space expected): %q", r, fastest)
			break
		}
	}
	if want := stripANSI(renderBar(r2, 400, cells)); fastest != want {
		t.Errorf("fastest row chart = %q, want %q", fastest, want)
	}
	// AC-1, AC-4: the bar is the one renderBar draws on the scale of 400.
	if want := stripANSI(renderBar(r1, 400, cells)); !strings.Contains(rows[2], want) {
		t.Errorf("row for 100 = %q, want it to contain the bar %q", rows[2], want)
	}
	if want := stripANSI(renderBar(r3, 400, cells)); !strings.Contains(rows[0], want) {
		t.Errorf("row for 50 = %q, want it to contain the bar %q", rows[0], want)
	}

	// AC-5, AC-16: every field of the text view stays.
	for i, rec := range []runRecord{r3, r2, r1} {
		mark := "✓"
		if !rec.Stable {
			mark = "⚠"
		}
		for _, want := range []string{formatMbps(rec.MedianMbps), "CV 1.5% " + mark, "5.5s"} {
			if !strings.Contains(rows[i], want) {
				t.Errorf("row %d = %q lacks %q", i, rows[i], want)
			}
		}
	}
}

// AC-6, AC-15, AC-16
func TestHistoryRowsChartThreshold(t *testing.T) {
	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	runs := []runRecord{testRecord(d1, 100), testRecord(d2, 200)}
	text := func(rec runRecord, median string) string {
		return rec.At.Local().Format(dateLayout) + "  " + median + "  CV 1.5% ✓  5.5s"
	}

	t.Run("one cell short of the minimum chart is today's text row", func(t *testing.T) {
		rows := plainRows(historyRows(runs, 54))
		want := []string{text(runs[1], "200.0 Mbps"), text(runs[0], "100.0 Mbps")}
		for i := range want {
			if rows[i] != want[i] {
				t.Errorf("row %d = %q, want exactly %q", i, rows[i], want[i])
			}
		}
	})

	t.Run("the minimum chart width draws an 8-cell chart", func(t *testing.T) {
		raw := historyRows(runs, 55)
		for i, row := range raw {
			if w := lipgloss.Width(row); w != 55 {
				t.Errorf("row %d is %d cells, want 55", i, w)
			}
		}
		rows := plainRows(raw)
		if got := string([]rune(rows[0])[18:26]); got != "▓▓▓▓▒▒▒▒" {
			t.Errorf("top row chart = %q, want %q", got, "▓▓▓▓▒▒▒▒")
		}
		if !hasBarGlyphs(rows[1]) {
			t.Errorf("row %q has no bar", rows[1])
		}
	})

	t.Run("every width from 32 to 100", func(t *testing.T) {
		for w := 32; w <= 100; w++ {
			rows := plainRows(historyRows(runs, w))
			bars := 0
			for i, row := range rows {
				if got := lipgloss.Width(row); got > w {
					t.Fatalf("width %d: row %d is %d cells", w, i, got)
				}
				if hasBarGlyphs(row) {
					bars++
				}
			}
			switch {
			case w < 55 && bars != 0:
				t.Errorf("width %d: %d rows have a bar, want none below 55", w, bars)
			case w >= 55 && bars != len(rows):
				t.Errorf("width %d: %d of %d rows have a bar, want all", w, bars, len(rows))
			}
		}
	})
}

// AC-12, AC-7
func TestHistoryRowsWithoutPositiveMedian(t *testing.T) {
	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

	t.Run("no row has a bar", func(t *testing.T) {
		rows := plainRows(historyRows([]runRecord{testRecord(d1, 0), testRecord(d2, 0)}, 100))
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		for i, row := range rows {
			if hasBarGlyphs(row) {
				t.Errorf("row %d has a bar: %q", i, row)
			}
			if !strings.Contains(row, "0.00 Mbps") {
				t.Errorf("row %d lacks the median: %q", i, row)
			}
		}
	})

	t.Run("a zero among positive medians draws no bar", func(t *testing.T) {
		rows := plainRows(historyRows([]runRecord{testRecord(d1, 0), testRecord(d2, 80)}, 100))
		const cells = 53
		got := string([]rune(rows[0])[18 : 18+cells])
		want := stripANSI(renderBar(testRecord(d2, 80), 80, cells))
		if got != want || strings.Contains(got, " ") || len([]rune(got)) != cells {
			t.Errorf("the 80 row chart = %q, want %d whole-cell glyphs equal to %q", got, cells, want)
		}
		if got := string([]rune(rows[1])[18 : 18+cells]); got != strings.Repeat(" ", cells) {
			t.Errorf("the 0 row chart = %q, want spaces", got)
		}
	})
}

// AC-10
func TestReadmeDescribesHistoryChart(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	_, section, ok := strings.Cut(doc, "## History")
	if !ok {
		t.Fatal("README has no History section")
	}
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	for _, want := range []string{"bar", "min", "max", "unstable", "white"} {
		if !strings.Contains(section, want) {
			t.Errorf("README History section does not mention %q", want)
		}
	}
}
