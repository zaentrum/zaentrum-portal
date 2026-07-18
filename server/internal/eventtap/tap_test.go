package eventtap

import (
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func TestParseExtractsAndRedacts(t *testing.T) {
	msg := kafka.Message{
		Topic:     "stube.catalog.item.discovered",
		Partition: 0,
		Offset:    42,
		Key:       []byte("item-7"),
		Time:      time.Unix(1700000000, 0).UTC(),
		Value:     []byte(`{"type":"discovered","itemId":"item-7","password":"hunter2","note":"ok"}`),
	}
	e := parse(msg)
	if e.Type != "discovered" {
		t.Errorf("type = %q, want discovered", e.Type)
	}
	if e.ItemID != "item-7" {
		t.Errorf("itemId = %q, want item-7", e.ItemID)
	}
	if strings.Contains(e.Payload, "hunter2") {
		t.Errorf("payload leaked the password: %s", e.Payload)
	}
	if !strings.Contains(e.Payload, "REDACTED") {
		t.Errorf("payload not redacted: %s", e.Payload)
	}
	if e.Size != len(msg.Value) {
		t.Errorf("size = %d, want %d", e.Size, len(msg.Value))
	}
}

func TestParseNonJSONPayload(t *testing.T) {
	e := parse(kafka.Message{Topic: "stube.x", Value: []byte("plain text log line")})
	if e.Type != "" || e.ItemID != "" {
		t.Errorf("expected no extracted fields, got type=%q itemId=%q", e.Type, e.ItemID)
	}
	if e.Payload != "plain text log line" {
		t.Errorf("payload = %q", e.Payload)
	}
}

func TestRingBufferCapAndOrder(t *testing.T) {
	tp := New(Config{Brokers: []string{"x:9092"}, Max: 3})
	for i := 0; i < 5; i++ {
		tp.add(Event{Topic: "t", Offset: int64(i)})
	}
	got := tp.Events("", 0)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (capped)", len(got))
	}
	// newest first: offsets 4,3,2
	if got[0].Offset != 4 || got[1].Offset != 3 || got[2].Offset != 2 {
		t.Errorf("order = %d,%d,%d, want 4,3,2", got[0].Offset, got[1].Offset, got[2].Offset)
	}
	// seq is monotonically assigned
	if got[0].Seq != 5 {
		t.Errorf("newest seq = %d, want 5", got[0].Seq)
	}
}

func TestEventsTopicFilterAndLimit(t *testing.T) {
	tp := New(Config{Brokers: []string{"x:9092"}, Max: 10})
	tp.add(Event{Topic: "a", Offset: 1})
	tp.add(Event{Topic: "b", Offset: 2})
	tp.add(Event{Topic: "a", Offset: 3})
	if got := tp.Events("a", 0); len(got) != 2 {
		t.Errorf("topic filter a: len = %d, want 2", len(got))
	}
	if got := tp.Events("", 1); len(got) != 1 || got[0].Offset != 3 {
		t.Errorf("limit 1: got %+v, want single newest (offset 3)", got)
	}
}

func TestUnavailableWithoutBrokers(t *testing.T) {
	if New(Config{}).Available() {
		t.Error("tap with no brokers should be unavailable")
	}
	if !New(Config{Brokers: []string{"k:9092"}}).Available() {
		t.Error("tap with brokers should be available")
	}
}
