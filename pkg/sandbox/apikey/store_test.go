package apikey

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseLegacyAndV2(t *testing.T) {
	legacy := []byte(`{"agent-a":"k8e-aaa","agent-b":"k8e-bbb"}`)
	recs, err := Parse(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if recs["agent-a"].Key != "k8e-aaa" || recs["agent-a"].ExpiresAt != nil {
		t.Fatalf("legacy record: %+v", recs["agent-a"])
	}

	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	rec := NewRecord("k8e-new", DefaultTTLDays, false, now)
	enc, err := Encode(map[string]Record{"n": rec})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back["n"].Key != "k8e-new" || back["n"].ExpiresAt == nil {
		t.Fatalf("v2 round-trip: %+v", back["n"])
	}
	wantExp := now.Add(30 * 24 * time.Hour)
	if !back["n"].ExpiresAt.Equal(wantExp) {
		t.Fatalf("expires: got %v want %v", back["n"].ExpiresAt, wantExp)
	}
}

func TestActiveSecretsFiltersExpired(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	keys := map[string]Record{
		"live":    {Key: "k8e-live", ExpiresAt: &future},
		"dead":    {Key: "k8e-dead", ExpiresAt: &past},
		"forever": {Key: "k8e-forever"},
	}
	active := ActiveSecrets(keys, now)
	if len(active) != 2 || active["live"] == "" || active["forever"] == "" || active["dead"] != "" {
		t.Fatalf("active=%v", active)
	}
}

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in    string
		days  int
		never bool
	}{
		{"", DefaultTTLDays, false},
		{"30d", 30, false},
		{"90d", 90, false},
		{"720h", 30, false},
		{"1h", 1, false},
		{"never", 0, true},
		{"0", 0, true},
		{"45", 45, false},
	}
	for _, tc := range cases {
		d, never, err := ParseTTL(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if d != tc.days || never != tc.never {
			t.Fatalf("%q: days=%d never=%v want days=%d never=%v", tc.in, d, never, tc.days, tc.never)
		}
	}
	if _, _, err := ParseTTL("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNewRecordNever(t *testing.T) {
	rec := NewRecord("k8e-x", 30, true, time.Now())
	if rec.ExpiresAt != nil || rec.TTLDays != 0 {
		t.Fatalf("never should omit expiry: %+v", rec)
	}
}

func TestTenantRoundTripsThroughV2(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	rec := NewRecordWithTenant("k8e-key", "acme", DefaultTTLDays, false, now)
	if rec.Tenant != "acme" {
		t.Fatalf("NewRecordWithTenant: got tenant %q", rec.Tenant)
	}

	enc, err := Encode(map[string]Record{"agent-a": rec})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back["agent-a"].Tenant != "acme" {
		t.Fatalf("tenant lost in round-trip: %+v", back["agent-a"])
	}
	if back["agent-a"].Key != "k8e-key" {
		t.Fatalf("key lost in round-trip: %+v", back["agent-a"])
	}
}

func TestTenantOmittedWhenEmpty(t *testing.T) {
	// An unbound key must stay wire-compatible with the pre-tenant format: no
	// "tenant" key in the JSON at all.
	rec := NewRecord("k8e-key", DefaultTTLDays, false, time.Now())
	enc, err := Encode(map[string]Record{"agent-a": rec})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(enc, &envelope); err != nil {
		t.Fatal(err)
	}
	var keys map[string]map[string]json.RawMessage
	if err := json.Unmarshal(envelope["keys"], &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["agent-a"]["tenant"]; ok {
		t.Fatalf("empty tenant must be omitted, got %s", enc)
	}
}
