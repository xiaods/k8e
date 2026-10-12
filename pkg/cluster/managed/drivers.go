package managed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

var (
	drivers []Driver
)

// ErrNotImplemented reports that a driver has no counterpart for an operation,
// as distinct from an operation that failed. A caller that would otherwise
// retry treats it as terminal: there is nothing to wait for, and retrying only
// produces noise. Tandem returns it for snapshot reconciliation, which has no
// meaning on a SQLite-backed store — retrying it once a second filled the log
// for the life of the process.
var ErrNotImplemented = errors.New("not implemented for this driver")

type Driver interface {
	SetControlConfig(config *config.Control) error
	IsInitialized() (bool, error)
	Register(handler http.Handler) (http.Handler, error)
	Reset(ctx context.Context, reboostrap func() error) error
	IsReset() (bool, error)
	ResetFile() string
	Start(ctx context.Context, clientAccessInfo *clientaccess.Info) error
	Test(ctx context.Context) error
	Restore(ctx context.Context) error
	EndpointName() string
	Snapshot(ctx context.Context) (*SnapshotResult, error)
	ReconcileSnapshotData(ctx context.Context) error
	GetMembersClientURLs(ctx context.Context) ([]string, error)
	RemoveSelf(ctx context.Context) error
}

func RegisterDriver(d Driver) {
	drivers = append(drivers, d)
}

func Registered() []Driver {
	return drivers
}

// Default returns the driver used when an operator has not chosen a backend.
// Embedded etcd stays the default until the M3 migration gate in issue #592.
func Default() Driver {
	return drivers[0]
}

// Select returns the driver an operator asked for by name. An empty name
// yields the default. An unknown name is an error rather than a silent
// fallback, so a typo in --datastore-backend cannot quietly select a different
// backend than the one that was requested.
func Select(name string) (Driver, error) {
	if name == "" {
		return Default(), nil
	}
	for _, driver := range drivers {
		if driver.EndpointName() == name {
			return driver, nil
		}
	}
	names := make([]string, 0, len(drivers))
	for _, driver := range drivers {
		names = append(names, driver.EndpointName())
	}
	return nil, fmt.Errorf("unknown datastore backend %q; available: %s", name, strings.Join(names, ", "))
}

// SnapshotResult is returned by the Snapshot function,
// and lists the names of created and deleted snapshots.
type SnapshotResult struct {
	Created []string `json:"created,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
}
