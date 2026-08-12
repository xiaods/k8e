// Package apikey provides the shared on-disk / Secret JSON codec for
// sandbox bootstrap API keys (KIP-17), including TTL metadata.
package apikey

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTTLDays is the create default when --ttl is omitted (KIP-17 / issue #538).
const DefaultTTLDays = 30

// FileVersion is the structured keys.json version.
const FileVersion = 2

// Record is one named API key with optional expiry.
type Record struct {
	Key       string     `json:"key"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	TTLDays   int        `json:"ttl_days,omitempty"`
}

// File is the v2 Secret payload.
type File struct {
	Version int               `json:"version"`
	Keys    map[string]Record `json:"keys"`
}

// Parse loads either v2 structured format or legacy flat map[string]string.
// Legacy keys have no expiry.
func Parse(data []byte) (map[string]Record, error) {
	if len(data) == 0 {
		return map[string]Record{}, nil
	}

	if looksStructured(data) {
		var v2 File
		if err := json.Unmarshal(data, &v2); err != nil {
			return nil, fmt.Errorf("parse api keys v2: %w", err)
		}
		if v2.Keys == nil {
			v2.Keys = map[string]Record{}
		}
		out := make(map[string]Record, len(v2.Keys))
		for name, rec := range v2.Keys {
			out[name] = rec
		}
		return out, nil
	}

	var legacy map[string]string
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("parse api keys: %w", err)
	}
	out := make(map[string]Record, len(legacy))
	for name, key := range legacy {
		out[name] = Record{Key: key}
	}
	return out, nil
}

func looksStructured(data []byte) bool {
	// legacy: {"a":"k8e-…"}  structured: {"version":2,"keys":{…}} or {"keys":{"a":{"key":"…"}}}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, hasKeys := raw["keys"]
	_, hasVersion := raw["version"]
	return hasKeys || hasVersion
}

// Encode serializes records as v2 JSON.
func Encode(keys map[string]Record) ([]byte, error) {
	if keys == nil {
		keys = map[string]Record{}
	}
	return json.MarshalIndent(File{Version: FileVersion, Keys: keys}, "", "  ")
}

// Expired reports whether the record is past expires_at.
func (r Record) Expired(now time.Time) bool {
	if r.ExpiresAt == nil {
		return false
	}
	return !now.Before(*r.ExpiresAt)
}

// ActiveSecrets returns name→secret for keys that are still valid at now.
func ActiveSecrets(keys map[string]Record, now time.Time) map[string]string {
	out := make(map[string]string, len(keys))
	for name, rec := range keys {
		if rec.Key == "" || rec.Expired(now) {
			continue
		}
		out[name] = rec.Key
	}
	return out
}

// NewRecord builds a record with optional TTL.
// never=true means no expiry; ttlDays is ignored.
func NewRecord(key string, ttlDays int, never bool, now time.Time) Record {
	rec := Record{
		Key:       key,
		CreatedAt: now.UTC(),
	}
	if never {
		return rec
	}
	if ttlDays <= 0 {
		ttlDays = DefaultTTLDays
	}
	rec.TTLDays = ttlDays
	exp := now.UTC().Add(time.Duration(ttlDays) * 24 * time.Hour)
	rec.ExpiresAt = &exp
	return rec
}

// ParseTTL parses CLI TTL strings.
// Supported: "", "30d", "90d", "720h", "30" (days), "0"/"never"/"none" (no expiry).
// Returns (ttlDays, never, error).
func ParseTTL(s string) (ttlDays int, never bool, err error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "":
		return DefaultTTLDays, false, nil
	case "never", "none", "0":
		return 0, true, nil
	}

	unit := "d"
	raw := s
	switch {
	case strings.HasSuffix(s, "d"):
		raw = strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "h"):
		unit = "h"
		raw = strings.TrimSuffix(s, "h")
	}

	n, e := strconv.Atoi(raw)
	if e != nil || n < 0 {
		return 0, false, fmt.Errorf("invalid ttl %q (want 30d, 720h, 30, or never)", s)
	}
	if n == 0 {
		return 0, true, nil
	}
	if unit == "h" {
		// Round up to whole days for storage; min 1 day if any hours remain.
		days := (n + 23) / 24
		if days < 1 {
			days = 1
		}
		return days, false, nil
	}
	return n, false, nil
}
