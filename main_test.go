package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AC-20
func TestPlainRunSavesToHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endlessBytes))
	defer srv.Close()
	cfg := testConfig(srv.URL)

	t.Run("a completed run is recorded", func(t *testing.T) {
		store := tempStore(t)
		if code := runPlain(cfg, store); code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
		doc := readDoc(t, store.path)
		if len(doc.Measurements) != 1 {
			t.Fatalf("file has %d records, want 1", len(doc.Measurements))
		}
		if doc.Measurements[0].URL != srv.URL {
			t.Fatalf("url = %q, want %q", doc.Measurements[0].URL, srv.URL)
		}
	})

	t.Run("a failing store does not change the exit code", func(t *testing.T) {
		if code := runPlain(cfg, blockedStore(t)); code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
	})
}

// AC-20
func TestRecordPlainRun(t *testing.T) {
	r := Result{Median: 300, Stable: true, Duration: 3 * time.Second}
	cfg := testConfig("https://host.example/f")

	t.Run("success is silent", func(t *testing.T) {
		store := tempStore(t)
		var w bytes.Buffer
		recordPlainRun(store, r, cfg, &w)
		if w.Len() != 0 {
			t.Fatalf("wrote %q, want nothing", w.String())
		}
		if doc := readDoc(t, store.path); len(doc.Measurements) != 1 {
			t.Fatalf("file has %d records, want 1", len(doc.Measurements))
		}
	})

	t.Run("failure writes exactly one warning line", func(t *testing.T) {
		var w bytes.Buffer
		recordPlainRun(blockedStore(t), r, cfg, &w)
		out := w.String()
		if !strings.HasPrefix(out, "warning: history not saved: ") || !strings.HasSuffix(out, "\n") {
			t.Fatalf("output = %q", out)
		}
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("output has more than one line: %q", out)
		}
	})
}
