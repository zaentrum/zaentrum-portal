package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

// A burst at once, then one per interval; each key its own bucket.
func TestBucketsPerKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := New(3, 10*time.Second).WithClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > 10*time.Second {
		t.Fatalf("past the burst: %v, wait %v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("another key shares the bucket")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("refilled, refused")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("one refill let two through")
	}
	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("after an hour, request %d refused", i+1)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("an idle hour filled more than the burst")
	}
}

// A flood of keys takes bounded memory: past the bound, a new key is refused
// until full buckets are swept.
func TestKeysAreBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := New(1, time.Second).WithClock(func() time.Time { return now })
	for i := 0; i < maxKeys; i++ {
		l.Allow(fmt.Sprint(i))
	}
	if ok, _ := l.Allow("one more"); ok {
		t.Error("a key past the bound was taken while every bucket is in use")
	}
	now = now.Add(2 * time.Second)
	if ok, _ := l.Allow("one more"); !ok {
		t.Error("full buckets were not swept")
	}
	if n := len(l.buckets); n > 1 {
		t.Errorf("%d buckets kept", n)
	}
}
