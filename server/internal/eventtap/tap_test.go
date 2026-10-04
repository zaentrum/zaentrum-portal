package eventtap

import (
	"encoding/json"
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

// The key is redacted by the rules the value is: a producer keys its messages
// by whatever it likes, and the console shows the key beside the payload.
func TestParseRedactsTheKey(t *testing.T) {
	for _, key := range []string{
		"token=k3y-s3cr3t",
		"Bearer k3y-s3cr3t-0123456789abcdef",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJrM3ktczNjcjN0In0.c2lnbmF0dXJlLWszeQ",
		"postgres://portal:k3y-s3cr3t@postgres:5432/portal",
	} {
		e := parse(kafka.Message{Topic: "stube.x", Key: []byte(key), Value: []byte(`{"type":"seen"}`)})
		if strings.Contains(e.Key, "k3y-s3cr3t") || strings.Contains(e.Key, "eyJzdWIi") {
			t.Errorf("key %q reaches the console as %q", key, e.Key)
		}
	}
	// An ordinary key stays as it is.
	if e := parse(kafka.Message{Topic: "stube.x", Key: []byte("item-7")}); e.Key != "item-7" {
		t.Errorf("key = %q", e.Key)
	}
}

// A JSON payload is redacted field by field: a quoted secret inside a string
// and an array under a credential-named field are gone, and what the console
// shows is still a document.
func TestParseRedactsADocumentFieldByField(t *testing.T) {
	e := parse(kafka.Message{Topic: "stube.x", Value: []byte(
		`{"type":"enriched","itemId":"item-7","note":"called with token=\"s3cr3t-a\"","tokens":["s3cr3t-b","s3cr3t-c"],"attempts":2}`)})
	if strings.Contains(e.Payload, "s3cr3t") {
		t.Errorf("payload leaked a secret: %s", e.Payload)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(e.Payload), &doc); err != nil {
		t.Fatalf("payload is no JSON: %v: %s", err, e.Payload)
	}
	if tokens, ok := doc["tokens"].([]any); !ok || len(tokens) != 2 {
		t.Errorf("tokens = %#v — the array keeps its shape", doc["tokens"])
	}
	if e.Type != "enriched" || e.ItemID != "item-7" || doc["attempts"] != float64(2) {
		t.Errorf("type = %q, itemId = %q, attempts = %#v", e.Type, e.ItemID, doc["attempts"])
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
