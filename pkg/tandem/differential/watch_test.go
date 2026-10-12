//go:build tandem_differential

package differential

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watchEvent is one normalized watch event. Arrival order and batch boundaries
// are not comparable — etcd and Tandem group events on their own timing — so
// the comparison key is what the event *was*, not when it turned up.
type watchEvent struct {
	Revision int64
	Key      string
	Value    string
	Type     string
	Version  int64
}

// collectWatch records a watch on a prefix, applies a known sequence of
// writes, and returns the events it saw.
func collectWatch(t *testing.T, ctx context.Context, client *clientv3.Client, apply func(context.Context) error) []watchEvent {
	t.Helper()
	watchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ch := client.Watch(watchCtx, "diff/watch/", clientv3.WithPrefix(), clientv3.WithPrevKV())
	// Give the watch time to register before the first write, or the first
	// event can be missed and the comparison becomes a race.
	time.Sleep(500 * time.Millisecond)

	if err := apply(ctx); err != nil {
		t.Fatalf("apply writes: %v", err)
	}

	var events []watchEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case response := <-ch:
			if response.Err() != nil {
				t.Fatalf("watch: %v", response.Err())
			}
			for _, event := range response.Events {
				events = append(events, normalizeEvent(event, response.Header.Revision))
			}
			if len(events) >= 3 {
				return sortEvents(events)
			}
		case <-deadline:
			t.Fatalf("watch delivered %d events, want 3: %v", len(events), events)
		}
	}
}

// normalizeEvent drops the watch's own header revision and uses the event's
// mod_revision instead, which is the revision the event was committed at and is
// therefore the thing both backends can agree on.
func normalizeEvent(event *mvccpb.Event, _ int64) watchEvent {
	name := "PUT"
	if event.Type == mvccpb.DELETE {
		name = "DELETE"
	}
	out := watchEvent{Type: name}
	if event.Kv != nil {
		out.Key = string(event.Kv.Key)
		out.Value = string(event.Kv.Value)
		out.Revision = event.Kv.ModRevision
		out.Version = event.Kv.Version
	}
	return out
}

func sortEvents(events []watchEvent) []watchEvent {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Revision != events[j].Revision {
			return events[i].Revision < events[j].Revision
		}
		return events[i].Key < events[j].Key
	})
	return events
}

func describeEvents(events []watchEvent) string {
	if len(events) == 0 {
		return "(none)"
	}
	out := ""
	for _, event := range events {
		out += fmt.Sprintf("\n    rev=%d %s %s=%s ver=%d", event.Revision, event.Type, event.Key, event.Value, event.Version)
	}
	return out
}

// TestDifferentialWatch covers KIP-29's event-order axis. The two backends
// batch differently, so the comparison is on the set of (key, type, value)
// a watcher observes, not on the order or grouping the events arrive in.
func TestDifferentialWatch(t *testing.T) {
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Each backend gets the identical write sequence applied to it alone.
	// Interleaving the two would make one store's revisions depend on the
	// other's traffic, and there would be nothing left to compare.
	apply := func(ctx context.Context, client *clientv3.Client) error {
		// A create, an update and a removal: the three shapes a controller's
		// cache has to reconcile.
		if _, err := client.Put(ctx, "diff/watch/a", "1"); err != nil {
			return err
		}
		if _, err := client.Put(ctx, "diff/watch/a", "2"); err != nil {
			return err
		}
		if _, err := client.Put(ctx, "diff/watch/b", "x"); err != nil {
			return err
		}
		if _, err := client.Delete(ctx, "diff/watch/b"); err != nil {
			return err
		}
		return nil
	}

	etcdEvents := collectWatch(t, ctx, h.Etcd.Client, func(ctx context.Context) error {
		return apply(ctx, h.Etcd.Client)
	})
	tandemEvents := collectWatch(t, ctx, h.Tandem.Client, func(ctx context.Context) error {
		return apply(ctx, h.Tandem.Client)
	})

	if difference := compareEvents(etcdEvents, tandemEvents); difference != "" {
		t.Errorf("watch event stream differs\n  %s", difference)
	}
}

func compareEvents(etcdEvents, tandemEvents []watchEvent) string {
	if len(etcdEvents) != len(tandemEvents) {
		return fmt.Sprintf("event count %d (etcd) vs %d (tandem)\n  etcd:   %s\n  tandem: %s",
			len(etcdEvents), len(tandemEvents), describeEvents(etcdEvents), describeEvents(tandemEvents))
	}
	for i := range etcdEvents {
		// Revisions are per-store, so compare their relative order and the
		// event's own content rather than the raw number.
		if etcdEvents[i].Type != tandemEvents[i].Type {
			return fmt.Sprintf("event %d is %s (etcd) vs %s (tandem)\n  etcd:   %s\n  tandem: %s",
				i, etcdEvents[i].Type, tandemEvents[i].Type, describeEvents(etcdEvents), describeEvents(tandemEvents))
		}
		if etcdEvents[i].Key != tandemEvents[i].Key {
			return fmt.Sprintf("event %d key %q (etcd) vs %q (tandem)\n  etcd:   %s\n  tandem: %s",
				i, etcdEvents[i].Key, tandemEvents[i].Key, describeEvents(etcdEvents), describeEvents(tandemEvents))
		}
		if etcdEvents[i].Value != tandemEvents[i].Value {
			return fmt.Sprintf("event %d value %q (etcd) vs %q (tandem)\n  etcd:   %s\n  tandem: %s",
				i, etcdEvents[i].Value, tandemEvents[i].Value, describeEvents(etcdEvents), describeEvents(tandemEvents))
		}
		if etcdEvents[i].Version != tandemEvents[i].Version {
			return fmt.Sprintf("event %d version %d (etcd) vs %d (tandem)\n  etcd:   %s\n  tandem: %s",
				i, etcdEvents[i].Version, tandemEvents[i].Version, describeEvents(etcdEvents), describeEvents(tandemEvents))
		}
	}
	return ""
}
