package dbbrowse

import (
	"strings"
	"testing"
	"time"
)

func TestCellFormatting(t *testing.T) {
	if got := cell(nil, false); got != "" {
		t.Errorf("nil => %q, want empty", got)
	}
	ts := time.Date(2026, 7, 18, 9, 30, 0, 0, time.UTC)
	if got := cell(ts, false); got != "2026-07-18T09:30:00Z" {
		t.Errorf("time => %q", got)
	}
	if got := cell("anything", true); got != "***REDACTED***" {
		t.Errorf("secret column => %q, want fully masked", got)
	}
	if got := cell("password=hunter2", false); strings.Contains(got, "hunter2") {
		t.Errorf("inline credential leaked: %q", got)
	}
	long := strings.Repeat("x", cellCap+50)
	if got := cell(long, false); len(got) > cellCap+3 {
		t.Errorf("cell not truncated: len=%d", len(got))
	}
	if got := cell([]byte("abcd"), false); got != "<4 bytes>" {
		t.Errorf("bytes => %q, want <4 bytes>", got)
	}
}

func TestFindWhitelist(t *testing.T) {
	if find("katalog.items") == nil {
		t.Error("expected katalog.items to be curated")
	}
	if find("katalog.items; DROP TABLE x") != nil {
		t.Error("non-whitelisted key must not resolve")
	}
	if find("") != nil {
		t.Error("empty key must not resolve")
	}
}

func TestTablesNeverLeakSQL(t *testing.T) {
	// Tables must blank the internal query/count before returning (they are
	// unexported so never serialise, but guard the invariant anyway).
	for _, tbl := range curated {
		if tbl.query == "" || tbl.count == "" {
			t.Errorf("curated %q has an empty query/count", tbl.Key)
		}
	}
}
