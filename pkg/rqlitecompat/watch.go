package rqlitecompat

import (
	"bytes"
	"context"
	"time"
)

// WatchCreate describes one watcher to add to a stream.
type WatchCreate struct {
	ID             int64
	Key            []byte
	RangeEnd       []byte
	StartRevision  int64
	ProgressNotify bool
	PrevKV         bool
	NoPut          bool
	NoDelete       bool
}

// WatchBatch is one WatchResponse worth of data for a single watcher.
type WatchBatch struct {
	WatchID         int64
	Revision        int64
	Created         bool
	Canceled        bool
	CompactRevision int64
	ProgressNotify  bool
	PrevKV          bool
	Events          []Event
}

type watcherState struct {
	WatchCreate
	lastRev int64
}

// watchSession tracks the watchers of one gRPC Watch stream. It performs no
// locking: the stream handler is the single goroutine that adds, cancels and
// polls, so the map is only ever touched by that goroutine.
type watchSession struct {
	store    *Store
	watchers map[int64]*watcherState
	order    []int64
	nextID   int64
	batch    int
}

func newWatchSession(store *Store) *watchSession {
	return &watchSession{store: store, watchers: map[int64]*watcherState{}, batch: 1000}
}

// add registers a watcher and returns the batch that must be sent immediately:
// either cancel (the start revision was compacted) or the created reply.
func (w *watchSession) add(ctx context.Context, c WatchCreate) (WatchBatch, error) {
	current, err := w.store.Revision(ctx)
	if err != nil {
		return WatchBatch{}, err
	}
	compact, err := w.store.CompactRevision(ctx)
	if err != nil {
		return WatchBatch{}, err
	}
	id := c.ID
	if id == 0 {
		w.nextID++
		id = w.nextID
	}
	c.ID = id

	if c.StartRevision != 0 && c.StartRevision < compact {
		return WatchBatch{WatchID: id, Canceled: true, CompactRevision: compact, Revision: compact}, nil
	}
	start := c.StartRevision
	if start == 0 {
		start = current + 1
	}
	ws := &watcherState{WatchCreate: c, lastRev: start - 1}
	w.watchers[id] = ws
	w.order = append(w.order, id)
	return WatchBatch{WatchID: id, Created: true, Revision: current}, nil
}

// cancel removes a watcher. It returns false when the id is unknown.
func (w *watchSession) cancel(id int64) bool {
	if _, ok := w.watchers[id]; !ok {
		return false
	}
	delete(w.watchers, id)
	for i, existing := range w.order {
		if existing == id {
			w.order = append(w.order[:i], w.order[i+1:]...)
			break
		}
	}
	return true
}

// poll reads new events for every watcher and returns the batches to send.
// Progress notifications are only emitted for watchers that asked for them and
// only when they received no event, so a progress revision never overtakes an
// event.
func (w *watchSession) poll(ctx context.Context) ([]WatchBatch, error) {
	if len(w.watchers) == 0 {
		return nil, nil
	}
	current, err := w.store.Revision(ctx)
	if err != nil {
		return nil, err
	}
	compact, err := w.store.CompactRevision(ctx)
	if err != nil {
		return nil, err
	}

	var batches []WatchBatch
	for _, id := range append([]int64(nil), w.order...) {
		ws, ok := w.watchers[id]
		if !ok {
			continue
		}
		if ws.lastRev < compact {
			batches = append(batches, WatchBatch{WatchID: id, Canceled: true, CompactRevision: compact})
			w.cancel(id)
			continue
		}
		var got []Event
		from := ws.lastRev
		for {
			events, more, err := w.store.EventsAfter(ctx, from, w.batch)
			if err != nil {
				return nil, err
			}
			for _, ev := range events {
				if ev.Revision <= ws.lastRev || !matches(ws, ev) {
					continue
				}
				got = append(got, ev)
				ws.lastRev = ev.Revision
			}
			if !more || len(events) == 0 {
				break
			}
			from = events[len(events)-1].Revision
		}
		if len(got) > 0 {
			batches = append(batches, WatchBatch{WatchID: id, Revision: ws.lastRev, PrevKV: ws.PrevKV, Events: got})
			continue
		}
		if ws.ProgressNotify && current > ws.lastRev {
			batches = append(batches, WatchBatch{WatchID: id, Revision: current, ProgressNotify: true})
		}
	}
	return batches, nil
}

func matches(w *watcherState, ev Event) bool {
	if ev.Type == EventPut && w.NoPut {
		return false
	}
	if ev.Type == EventDelete && w.NoDelete {
		return false
	}
	return keyInRange(ev.KV.Key, w.Key, w.RangeEnd)
}

// keyInRange reports whether k is in [key, rangeEnd), with the same encoding
// as a Range request: an empty rangeEnd is an exact match and a one-byte zero
// rangeEnd is unbounded.
func keyInRange(k, key, rangeEnd []byte) bool {
	if len(rangeEnd) == 0 {
		return bytes.Equal(k, key)
	}
	if bytes.Compare(k, key) < 0 {
		return false
	}
	if len(rangeEnd) == 1 && rangeEnd[0] == 0 {
		return true
	}
	return bytes.Compare(k, rangeEnd) < 0
}

// DefaultWatchPollInterval is how often a stream polls for new events. The
// KIP-29 baseline is a shared event reader pulled by revision, not a
// per-watcher notification fabric.
const DefaultWatchPollInterval = 20 * time.Millisecond
