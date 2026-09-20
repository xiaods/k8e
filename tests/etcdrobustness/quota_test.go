package etcdrobustness

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// quotaTestSize is small enough to fill in a few hundred writes.
const quotaTestSize = 8 << 20

// TestEmbeddedEtcdQuotaExhaustionRefusesWrites walks the disk-pressure
// scenario: the store hits its quota, writes are refused with NOSPACE,
// acknowledged data is still readable and intact, and the store becomes
// writable again after delete + compact + defragment + alarm disarm.
//
// Only the observable contract is asserted: the refusal error, the alarm, the
// intact acknowledged data and a successful write after the repair. The backend
// size is left out of the assertions on purpose: etcd refuses the write that
// would cross the quota, and the size it reports is not tied to the quota.
func TestEmbeddedEtcdQuotaExhaustionRefusesWrites(t *testing.T) {
	dir := workDir(t)
	clientURL, peerURL := reserveURLs(t)
	startNode(t, NodeOptions{
		Name:       "robustness",
		Dir:        dir,
		ClientURL:  clientURL,
		PeerURL:    peerURL,
		QuotaBytes: quotaTestSize,
	})
	client := mustClient(t, clientURL)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	historyPath := filepath.Join(dir, "history.jsonl")
	recorder, err := NewRecorder(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	value := strings.Repeat("q", 64*1024)
	const keys = 8
	var quotaErr error
	acknowledged := 0
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("quota/%06d", i%keys)
		attempt, err := recorder.Begin(Operation{Kind: KindPut, Key: key, Value: value, Endpoint: clientURL})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Put(ctx, key, value)
		if err != nil {
			if err := attempt.Reject(err); err != nil {
				t.Fatal(err)
			}
			quotaErr = err
			break
		}
		if err := attempt.Acknowledge(response.Header.Revision); err != nil {
			t.Fatal(err)
		}
		acknowledged++
	}

	if quotaErr == nil {
		t.Fatalf("the %d byte quota never refused a write after %d acknowledged writes", quotaTestSize, acknowledged)
	}
	if !strings.Contains(quotaErr.Error(), "database space exceeded") {
		t.Fatalf("quota refusal = %v, want etcdserver: mvcc: database space exceeded", quotaErr)
	}
	if acknowledged == 0 {
		t.Fatal("no write was acknowledged before the quota was hit")
	}

	// The refusal plus the alarm are the contract. etcd refuses the write that
	// would cross the quota, and the backend size it reports is not tied to the
	// quota (it can sit either side of it), so the size is logged, not asserted.
	status, err := client.Status(ctx, clientURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quota %d bytes, db size %d bytes, in use %d bytes: %v", quotaTestSize, status.DbSize, status.DbSizeInUse, quotaErr)
	alarms, err := client.AlarmList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoSpaceAlarm(alarms) {
		t.Fatalf("expected an active NOSPACE alarm, got %+v", alarms.Alarms)
	}

	// A full store must not have lost anything it already acknowledged.
	history, err := LoadHistory(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, history, ClientReader(client))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("acknowledged data lost under quota pressure:\n%s", strings.Join(report.Violations, "\n"))
	}
	if report.Checked < 2 {
		t.Fatalf("oracle checked only %d keys; the pressure scenario did not exercise enough data", report.Checked)
	}

	// The documented repair: delete, compact, defragment, then disarm NOSPACE.
	if _, err := client.Delete(ctx, "quota/", clientv3.WithPrefix()); err != nil {
		t.Fatalf("delete on a full store: %v", err)
	}
	status, err = client.Status(ctx, clientURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Compact(ctx, status.Header.Revision); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := client.Defragment(ctx, clientURL); err != nil {
		t.Fatalf("defragment: %v", err)
	}
	if _, err := client.AlarmDisarm(ctx, &clientv3.AlarmMember{
		MemberID: status.Header.MemberId,
		Alarm:    etcdserverpb.AlarmType_NOSPACE,
	}); err != nil {
		t.Fatalf("disarm NOSPACE: %v", err)
	}

	attempt, err := recorder.Begin(Operation{Kind: KindPut, Key: "after/maintenance", Value: "writable", Endpoint: clientURL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Put(ctx, "after/maintenance", "writable")
	if err != nil {
		_ = attempt.Unknown(err)
		t.Fatalf("write after maintenance: %v", err)
	}
	if err := attempt.Acknowledge(response.Header.Revision); err != nil {
		t.Fatal(err)
	}

	after, err := client.Status(ctx, clientURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after maintenance: db size %d bytes, in use %d bytes", after.DbSize, after.DbSizeInUse)
	alarms, err = client.AlarmList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoSpaceAlarm(alarms) {
		t.Fatalf("the NOSPACE alarm is still active after disarm: %+v", alarms.Alarms)
	}
}

func hasNoSpaceAlarm(list *clientv3.AlarmResponse) bool {
	if list == nil {
		return false
	}
	for _, alarm := range list.Alarms {
		if alarm.Alarm == etcdserverpb.AlarmType_NOSPACE {
			return true
		}
	}
	return false
}
