package managed

import (
	"context"
	"net/http"
	"testing"

	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

// stubDriver is a minimal Driver used to exercise registration and selection
// without starting a real datastore.
type stubDriver struct {
	name          string
	initialized   bool
	started       int
	configApplied int
}

func (s *stubDriver) SetControlConfig(*config.Control) error { s.configApplied++; return nil }
func (s *stubDriver) IsInitialized() (bool, error)           { return s.initialized, nil }
func (s *stubDriver) Register(h http.Handler) (http.Handler, error) {
	return h, nil
}
func (s *stubDriver) Reset(context.Context, func() error) error      { return nil }
func (s *stubDriver) IsReset() (bool, error)                         { return false, nil }
func (s *stubDriver) ResetFile() string                              { return "" }
func (s *stubDriver) Start(context.Context, *clientaccess.Info) error { s.started++; return nil }
func (s *stubDriver) Test(context.Context) error                     { return nil }
func (s *stubDriver) Restore(context.Context) error                  { return nil }
func (s *stubDriver) EndpointName() string                           { return s.name }
func (s *stubDriver) Snapshot(context.Context) (*SnapshotResult, error) {
	return nil, nil
}
func (s *stubDriver) ReconcileSnapshotData(context.Context) error       { return nil }
func (s *stubDriver) GetMembersClientURLs(context.Context) ([]string, error) {
	return nil, nil
}
func (s *stubDriver) RemoveSelf(context.Context) error { return nil }

// withDrivers swaps the registry for the duration of a test.
func withDrivers(t *testing.T, stubs ...Driver) {
	t.Helper()
	saved := drivers
	drivers = stubs
	t.Cleanup(func() { drivers = saved })
}

func TestSelectDefaultsToFirstRegistered(t *testing.T) {
	withDrivers(t, &stubDriver{name: "etcd"}, &stubDriver{name: "tandem"})
	selected, err := Select("")
	if err != nil {
		t.Fatalf("Select(\"\"): %v", err)
	}
	if selected.EndpointName() != "etcd" {
		t.Fatalf("default backend = %q, want etcd", selected.EndpointName())
	}
}

func TestSelectByExplicitName(t *testing.T) {
	withDrivers(t, &stubDriver{name: "etcd"}, &stubDriver{name: "tandem"})
	selected, err := Select("tandem")
	if err != nil {
		t.Fatalf("Select(\"tandem\"): %v", err)
	}
	if selected.EndpointName() != "tandem" {
		t.Fatalf("selected %q, want tandem", selected.EndpointName())
	}
}

func TestSelectRejectsUnknownBackend(t *testing.T) {
	withDrivers(t, &stubDriver{name: "etcd"}, &stubDriver{name: "tandem"})
	selected, err := Select("nope")
	if err == nil {
		t.Fatalf("expected an error for an unknown backend, got %v", selected)
	}
	if selected != nil {
		t.Fatal("an unknown backend must not fall back to a default")
	}
}
