//go:build tandem_differential

package differential

import (
	"context"
	"fmt"
	"testing"
	"time"

	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// collectRangeStream drains a GetStream channel into a slice, blocking until
// the stream closes or the context expires.
func collectRangeStream(t *testing.T, ctx context.Context, ch clientv3.GetStreamChan) []*clientv3.RangeStreamResponse {
	t.Helper()
	var out []*clientv3.RangeStreamResponse
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return out
			}
			if chunk.Err() != nil {
				t.Fatalf("range stream error: %v", chunk.Err())
			}
			out = append(out, &chunk)
		case <-ctx.Done():
			t.Fatalf("range stream did not finish: %v", ctx.Err())
		}
	}
}

// mergeStreamChunks applies the same merge etcd's consumer does: every chunk's
// Kvs are appended, and the terminal chunk's Header/More/Count win. This is
// what a caller that consumes a RangeStream reconstructs into a single answer.
func mergeStreamChunks(chunks []*clientv3.RangeStreamResponse) *etcdserverpb.RangeResponse {
	var kvs []*mvccpb.KeyValue
	var header *etcdserverpb.ResponseHeader
	var more bool
	var count int64
	for _, chunk := range chunks {
		if chunk == nil || chunk.RangeResponse == nil {
			continue
		}
		kvs = append(kvs, chunk.RangeResponse.Kvs...)
		header = chunk.RangeResponse.Header
		more = chunk.RangeResponse.More
		count = chunk.RangeResponse.Count
	}
	return &etcdserverpb.RangeResponse{Header: header, Kvs: kvs, More: more, Count: count}
}

// seedStreamKeys writes n sequential keys to both backends so the stream test
// has a known set to read back.
func seedStreamKeys(t *testing.T, ctx context.Context, h *Harness, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("stream/key/%04d", i)
		value := fmt.Sprintf("v-%d", i)
		if _, err := h.Etcd.Client.Put(ctx, key, value); err != nil {
			t.Fatalf("seed etcd: %v", err)
		}
		if _, err := h.Tandem.Client.Put(ctx, key, value); err != nil {
			t.Fatalf("seed tandem: %v", err)
		}
	}
}

// streamResult holds the merged stream and the channel count from one backend.
type streamResult struct {
	merged  *etcdserverpb.RangeResponse
	chunks  int
}

// runStream collects a GetStream from a client and merges the chunks.
func runStream(t *testing.T, ctx context.Context, client *clientv3.Client) streamResult {
	t.Helper()
	stream, err := client.GetStream(ctx, "stream/key/",
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	chunks := collectRangeStream(t, ctx, stream)
	return streamResult{merged: mergeStreamChunks(chunks), chunks: len(chunks)}
}

// assertStreamMatchesUnary verifies the merged stream matches the unary range.
func assertStreamMatchesUnary(t *testing.T, merged *etcdserverpb.RangeResponse, unaryCount int64) {
	t.Helper()
	if int64(len(merged.Kvs)) != unaryCount {
		t.Fatalf("stream key count %d != unary count %d", len(merged.Kvs), unaryCount)
	}
	if merged.More {
		t.Fatalf("terminal chunk has more=true")
	}
}

// assertStreamsAgree verifies etcd and Tandem merged streams agree on keys/values.
func assertStreamsAgree(t *testing.T, etcdMerged, tandemMerged *etcdserverpb.RangeResponse) {
	t.Helper()
	if len(etcdMerged.Kvs) != len(tandemMerged.Kvs) {
		t.Fatalf("stream key count mismatch: etcd=%d tandem=%d", len(etcdMerged.Kvs), len(tandemMerged.Kvs))
	}
	for i := range etcdMerged.Kvs {
		if string(etcdMerged.Kvs[i].Key) != string(tandemMerged.Kvs[i].Key) {
			t.Fatalf("key %d mismatch: etcd=%q tandem=%q", i, etcdMerged.Kvs[i].Key, tandemMerged.Kvs[i].Key)
		}
		if string(etcdMerged.Kvs[i].Value) != string(tandemMerged.Kvs[i].Value) {
			t.Fatalf("value %d mismatch: etcd=%q tandem=%q", i, etcdMerged.Kvs[i].Value, tandemMerged.Kvs[i].Value)
		}
	}
}

// TestDifferentialRangeStream compares Tandem's RangeStream output against
// embedded etcd's. It writes a known set of keys, then streams them back and
// confirms the merged result matches what the unary Range returns on both
// backends — and that the two backends agree.
func TestDifferentialRangeStream(t *testing.T) {
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	seedStreamKeys(t, ctx, h, 50)

	// Baseline: the unary Range on each backend must agree.
	etcdUnary, err := h.Etcd.Client.Get(ctx, "stream/key/",
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		t.Fatalf("unary range etcd: %v", err)
	}
	tandemUnary, err := h.Tandem.Client.Get(ctx, "stream/key/",
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		t.Fatalf("unary range tandem: %v", err)
	}
	if etcdUnary.Count != tandemUnary.Count {
		t.Fatalf("unary count mismatch: etcd=%d tandem=%d", etcdUnary.Count, tandemUnary.Count)
	}

	etcdResult := runStream(t, ctx, h.Etcd.Client)
	tandemResult := runStream(t, ctx, h.Tandem.Client)

	assertStreamMatchesUnary(t, etcdResult.merged, etcdUnary.Count)
	assertStreamMatchesUnary(t, tandemResult.merged, tandemUnary.Count)
	assertStreamsAgree(t, etcdResult.merged, tandemResult.merged)

	t.Logf("RangeStream: etcd chunks=%d tandem chunks=%d keys=%d",
		etcdResult.chunks, tandemResult.chunks, len(tandemResult.merged.Kvs))
}
