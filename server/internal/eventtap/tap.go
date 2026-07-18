// Package eventtap provides a read-only "event tap" for the admin debug console:
// a background consumer that tails the platform's Kafka topics into a bounded
// ring buffer, plus a topology view (topics, partitions, and the consumer groups
// bound to each). It never produces, and consumes under a single stable group at
// the log HEAD, so it observes live events without disturbing the pipeline's own
// committed offsets. Everything degrades to "unavailable" when KAFKA_BROKERS is
// unset (dev / an appliance without the bus wired), keeping the service healthy.
package eventtap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
)

const (
	defaultMax        = 500     // ring-buffer capacity
	payloadCap        = 4000    // bytes of (scrubbed) payload kept per event
	tapGroupID        = "portal-event-tap"
	defaultPrefix     = "stube." // the platform's topic namespace
	discoverEvery     = 15 * time.Second
	discoverAttempts  = 40 // ~10 min of waiting for topics to appear
)

// Config wires the tap to the bus. Brokers empty => the tap is inert (Available
// reports false). TLS is optional (nil => PLAINTEXT, the in-cluster demo).
type Config struct {
	Brokers     []string
	TLS         *tls.Config
	TopicPrefix string
	Max         int
}

// Event is one observed Kafka message, normalised for the console. Payload is the
// message value with secrets redacted and capped; Type/ItemID are best-effort
// pulls from a JSON body so the console can summarise without showing everything.
type Event struct {
	Seq       uint64    `json:"seq"`
	Topic     string    `json:"topic"`
	Partition int       `json:"partition"`
	Offset    int64     `json:"offset"`
	Key       string    `json:"key"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type,omitempty"`
	ItemID    string    `json:"itemId,omitempty"`
	Payload   string    `json:"payload"`
	Size      int       `json:"size"`
}

// TopicInfo describes one topic for the topology view.
type TopicInfo struct {
	Topic      string   `json:"topic"`
	Partitions int      `json:"partitions"`
	Consumers  []string `json:"consumers"`          // consumer groups bound to it (live)
	Seen       int      `json:"seen"`               // events observed by the tap this session
	LastEvent  string   `json:"lastEvent,omitempty"` // RFC3339 of the most recent observed event
}

// Topology is the whole introspection view: topics + their live consumer groups.
type Topology struct {
	Available bool        `json:"available"`
	Brokers   []string    `json:"brokers"`
	Topics    []TopicInfo `json:"topics"`
	Groups    []string    `json:"groups"`
	Note      string      `json:"note,omitempty"`
}

// Tap tails the bus into a ring buffer and answers topology queries.
type Tap struct {
	cfg  Config
	mu   sync.RWMutex
	buf  []Event
	seq  uint64
	seen map[string]int
	last map[string]time.Time
}

// New constructs an (unstarted) tap. Call Start in a goroutine to begin tailing.
func New(cfg Config) *Tap {
	if cfg.Max <= 0 {
		cfg.Max = defaultMax
	}
	if strings.TrimSpace(cfg.TopicPrefix) == "" {
		cfg.TopicPrefix = defaultPrefix
	}
	return &Tap{cfg: cfg, seen: map[string]int{}, last: map[string]time.Time{}}
}

// Available reports whether the tap is wired to a bus (has brokers).
func (t *Tap) Available() bool { return t != nil && len(t.cfg.Brokers) > 0 }

// Start tails the platform topics until ctx is cancelled. Defensive throughout:
// no brokers / no topics / read errors are logged and retried, never fatal.
func (t *Tap) Start(ctx context.Context) {
	if !t.Available() {
		log.Printf("kafka tap: no brokers configured; event console will report unavailable")
		return
	}
	topics := t.waitForTopics(ctx)
	if len(topics) == 0 {
		log.Printf("kafka tap: no %q topics found; tap idle (topology still lists groups)", t.cfg.TopicPrefix)
		return
	}

	dialer := &kafka.Dialer{Timeout: 10 * time.Second, DualStack: true}
	if t.cfg.TLS != nil {
		dialer.TLS = t.cfg.TLS
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     t.cfg.Brokers,
		GroupID:     tapGroupID,
		GroupTopics: topics,
		Dialer:      dialer,
		StartOffset: kafka.LastOffset, // live tail (only applies on a fresh group)
	})
	defer reader.Close()
	log.Printf("kafka tap active on %d topics: %s", len(topics), strings.Join(topics, ","))

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("kafka tap: read error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		t.add(parse(msg))
	}
}

// waitForTopics polls metadata until at least one prefixed topic exists (the demo
// creates topics lazily on first produce) or ctx ends / attempts are exhausted.
func (t *Tap) waitForTopics(ctx context.Context) []string {
	for i := 0; i < discoverAttempts; i++ {
		if topics := t.discoverTopics(ctx); len(topics) > 0 {
			return topics
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(discoverEvery):
		}
	}
	return t.discoverTopics(ctx)
}

func (t *Tap) discoverTopics(ctx context.Context) []string {
	md, err := t.client().Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil
	}
	var out []string
	for _, tp := range md.Topics {
		if tp.Error != nil {
			continue
		}
		if strings.HasPrefix(tp.Name, t.cfg.TopicPrefix) {
			out = append(out, tp.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (t *Tap) client() *kafka.Client {
	c := &kafka.Client{Addr: kafka.TCP(t.cfg.Brokers...), Timeout: 10 * time.Second}
	if t.cfg.TLS != nil {
		c.Transport = &kafka.Transport{TLS: t.cfg.TLS}
	}
	return c
}

// add appends an event to the ring buffer (bounded to cfg.Max, newest kept).
func (t *Tap) add(e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	e.Seq = t.seq
	t.buf = append(t.buf, e)
	if len(t.buf) > t.cfg.Max {
		t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-t.cfg.Max:]...)
	}
	t.seen[e.Topic]++
	t.last[e.Topic] = e.Time
}

// Events returns up to limit most-recent events (newest first), optionally
// filtered to a single topic. limit<=0 returns all buffered events.
func (t *Tap) Events(topic string, limit int) []Event {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.buf) {
		limit = len(t.buf)
	}
	out := make([]Event, 0, limit)
	for i := len(t.buf) - 1; i >= 0 && len(out) < limit; i-- {
		if topic != "" && t.buf[i].Topic != topic {
			continue
		}
		out = append(out, t.buf[i])
	}
	return out
}

// Topology returns the live topic/consumer-group view, enriched with the tap's
// own observed activity. Every broker call is best-effort: a failure degrades the
// affected section rather than the whole response.
func (t *Tap) Topology(ctx context.Context) Topology {
	if !t.Available() {
		return Topology{Available: false, Note: "Kafka introspection is unavailable (KAFKA_BROKERS unset)"}
	}
	client := t.client()
	top := Topology{Available: true, Brokers: t.cfg.Brokers}

	byTopic := map[string]*TopicInfo{}
	if md, err := client.Metadata(ctx, &kafka.MetadataRequest{}); err == nil {
		for _, tp := range md.Topics {
			if tp.Error != nil || !strings.HasPrefix(tp.Name, t.cfg.TopicPrefix) {
				continue
			}
			byTopic[tp.Name] = &TopicInfo{Topic: tp.Name, Partitions: len(tp.Partitions)}
		}
	} else {
		top.Note = "metadata: " + err.Error()
	}

	// Consumer groups → the topics they are assigned. ListGroups is cheap;
	// DescribeGroups yields member assignments (topic list) per group.
	if lg, err := client.ListGroups(ctx, &kafka.ListGroupsRequest{}); err == nil {
		ids := make([]string, 0, len(lg.Groups))
		for _, g := range lg.Groups {
			ids = append(ids, g.GroupID)
		}
		sort.Strings(ids)
		top.Groups = ids
		if len(ids) > 0 {
			if dg, err := client.DescribeGroups(ctx, &kafka.DescribeGroupsRequest{GroupIDs: ids}); err == nil {
				for _, g := range dg.Groups {
					for _, mem := range g.Members {
						for _, at := range mem.MemberAssignments.Topics {
							if ti := byTopic[at.Topic]; ti != nil {
								ti.Consumers = appendUnique(ti.Consumers, g.GroupID)
							}
						}
					}
				}
			}
		}
	}

	// Fold in what the tap has observed this session.
	t.mu.RLock()
	for name, ti := range byTopic {
		ti.Seen = t.seen[name]
		if ts, ok := t.last[name]; ok {
			ti.LastEvent = ts.UTC().Format(time.RFC3339)
		}
	}
	t.mu.RUnlock()

	names := make([]string, 0, len(byTopic))
	for n := range byTopic {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ti := byTopic[n]
		sort.Strings(ti.Consumers)
		top.Topics = append(top.Topics, *ti)
	}
	return top
}

// parse normalises a Kafka message: it pulls a Type/ItemID from a JSON body when
// present, compacts + redacts the payload, and caps its length.
func parse(msg kafka.Message) Event {
	e := Event{
		Topic:     msg.Topic,
		Partition: msg.Partition,
		Offset:    msg.Offset,
		Key:       string(msg.Key),
		Time:      msg.Time,
		Size:      len(msg.Value),
	}
	payload := string(msg.Value)
	var m map[string]any
	if json.Unmarshal(msg.Value, &m) == nil {
		e.Type = firstString(m, "type", "eventType", "event", "action", "status")
		e.ItemID = firstString(m, "itemId", "item_id", "itemID", "id")
		if b, err := json.Marshal(m); err == nil {
			payload = string(b)
		}
	}
	payload = redact.Secrets(payload)
	if len(payload) > payloadCap {
		payload = payload[:payloadCap] + "…(truncated)"
	}
	e.Payload = payload
	return e
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
