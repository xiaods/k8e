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

// quotaKeys is how many distinct keys the fill loop rotates through.
const quotaKeys = 8

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
	recorder := mustRecorder(t, historyPath, time.Now)
	defer recorder.Close()

	acknowledged, quotaErr := fillQuota(t, ctx, client, recorder, clientURL)
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
	status := mustStatus(t, ctx, client, clientURL)
	t.Logf("quota %d bytes, db size %d bytes, in use %d bytes: %v", quotaTestSize, status.DbSize, status.DbSizeInUse, quotaErr)
	alarms := mustAlarms(t, ctx, client)
	if !hasNoSpaceAlarm(alarms) {
		t.Fatalf("expected an active NOSPACE alarm, got %+v", alarms.Alarms)
	}

	// A full store must not have lost anything it already acknowledged.
	verifyAcknowledgedIntact(t, ctx, client, historyPath)

	// The documented repair: delete, compact, defragment, then disarm NOSPACE.
	repairFullStore(t, ctx, client, clientURL, status)

	writeAfterMaintenance(t, ctx, client, recorder, clientURL)
	after := mustStatus(t, ctx, client, clientURL)
	t.Logf("after maintenance: db size %d bytes, in use %d bytes", after.DbSize, after.DbSizeInUse)
	if hasNoSpaceAlarm(mustAlarms(t, ctx, client)) {
		t.Fatal("the NOSPACE alarm is still active after disarm")
	}
}

// fillQuota writes fixed-size values until the store refuses one, and returns
// how many writes were acknowledged together with the refusal.
func fillQuota(t *testing.T, ctx context.Context, client *clientv3.Client, recorder *Recorder, endpoint string) (int, error) {
	t.Helper()
	value := strings.Repeat("q", 64*1024)
	acknowledged := 0
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("quota/%06d", i%quotaKeys)
		attempt := mustBegin(t, recorder, Operation{Kind: KindPut, Key: key, Value: value, Endpoint: endpoint})
		response, err := client.Put(ctx, key, value)
		if err != nil {
			must(t, attempt.Reject(err))
			return acknowledged, err
		}
		must(t, attempt.Acknowledge(response.Header.Revision))
		acknowledged++
	}
	return acknowledged, nil
}

// verifyAcknowledgedIntact checks that the full store still holds everything
// the history acknowledged and that the oracle saw enough data to be meaningful.
func verifyAcknowledgedIntact(t *testing.T, ctx context.Context, client *clientv3.Client, historyPath string) {
	t.Helper()
	history := mustLoadHistory(t, historyPath)
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
}

// repairFullStore runs the maintenance sequence for a store that hit its quota:
// delete the data, compact, defragment and disarm the NOSPACE alarm.
func repairFullStore(t *testing.T, ctx context.Context, client *clientv3.Client, endpoint string, status *clientv3.StatusResponse) {
	t.Helper()
	if _, err := client.Delete(ctx, "quota/", clientv3.WithPrefix()); err != nil {
		t.Fatalf("delete on a full store: %v", err)
	}
	after, err := client.Status(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Compact(ctx, after.Header.Revision); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := client.Defragment(ctx, endpoint); err != nil {
		t.Fatalf("defragment: %v", err)
	}
	if _, err := client.AlarmDisarm(ctx, &clientv3.AlarmMember{
		MemberID: status.Header.MemberId,
		Alarm:    etcdserverpb.AlarmType_NOSPACE,
	}); err != nil {
		t.Fatalf("disarm NOSPACE: %v", err)
	}
}

// writeAfterMaintenance proves the repaired store accepts a new write.
func writeAfterMaintenance(t *testing.T, ctx context.Context, client *clientv3.Client, recorder *Recorder, endpoint string) {
	t.Helper()
	attempt := mustBegin(t, recorder, Operation{Kind: KindPut, Key: "after/maintenance", Value: "writable", Endpoint: endpoint})
	response, err := client.Put(ctx, "after/maintenance", "writable")
	if err != nil {
		_ = attempt.Unknown(err)
		t.Fatalf("write after maintenance: %v", err)
	}
	must(t, attempt.Acknowledge(response.Header.Revision))
}

func mustStatus(t *testing.T, ctx context.Context, client *clientv3.Client, endpoint string) *clientv3.StatusResponse {
	t.Helper()
	status, err := client.Status(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func mustAlarms(t *testing.T, ctx context.Context, client *clientv3.Client) *clientv3.AlarmResponse {
	t.Helper()
	alarms, err := client.AlarmList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return alarms
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
