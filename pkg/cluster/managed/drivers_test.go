package managed

import (
	"context"
	"errors"
	"fmt"
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
func (s *stubDriver) Reset(context.Context, func() error) error       { return nil }
func (s *stubDriver) IsReset() (bool, error)                          { return false, nil }
func (s *stubDriver) ResetFile() string                               { return "" }
func (s *stubDriver) Start(context.Context, *clientaccess.Info) error { s.started++; return nil }
func (s *stubDriver) Test(context.Context) error                      { return nil }
func (s *stubDriver) Restore(context.Context) error                   { return nil }
func (s *stubDriver) EndpointName() string                            { return s.name }
func (s *stubDriver) Snapshot(context.Context) (*SnapshotResult, error) {
	return nil, nil
}
func (s *stubDriver) ReconcileSnapshotData(context.Context) error { return nil }
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

// A driver with no counterpart for an operation must be distinguishable from
// one that failed, because the snapshot reconcile loop stops on the first and
// retries the second. The Tandem store has no snapshot records to write, and
// retrying that once a second filled the log for the life of the process.
func TestErrNotImplementedIsDistinguishableFromFailure(t *testing.T) {
	driver := &notImplementedDriver{stubDriver: stubDriver{name: "tandem"}}
	err := driver.ReconcileSnapshotData(context.Background())
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("ReconcileSnapshotData returned %v, want an error wrapping ErrNotImplemented", err)
	}
	// A genuine failure must not be mistaken for the terminal case, or the
	// loop would give up on a datastore that is merely broken.
	failure := &failingDriver{stubDriver: stubDriver{name: "tandem"}}
	if errors.Is(failure.ReconcileSnapshotData(context.Background()), ErrNotImplemented) {
		t.Fatal("a reconcile failure must not satisfy errors.Is(ErrNotImplemented)")
	}
}

type notImplementedDriver struct{ stubDriver }

func (d *notImplementedDriver) ReconcileSnapshotData(context.Context) error {
	return fmt.Errorf("reconcile: %w", ErrNotImplemented)
}

type failingDriver struct{ stubDriver }

func (d *failingDriver) ReconcileSnapshotData(context.Context) error {
	return errors.New("datastore unreachable")
}
