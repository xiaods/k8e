// Package netsy integrates the Netsy datastore (https://netsy.dev) with k8e.
//
// Netsy is a replicated key-value database that persists to object storage and
// implements the subset of the etcd v3 gRPC API used by Kubernetes (Range, Txn,
// Watch, MemberList, Status). k8e already supports an external etcd-compatible
// datastore through --datastore-endpoint; this package adds the missing pieces
// needed to run Netsy itself as the datastore:
//
//   - rendering the Netsy JSONC cluster config and per-node environment,
//   - generating the Netsy mTLS PKI (Netsy requires TLS 1.3 and netsy:// URI
//     SANs on every certificate), and
//   - supervising the netsy process until its client API is healthy.
//
// See docs/kip-29-replace-etcd-with-netsy.md.
package netsy

// Defaults for the Netsy per-node settings. Ports follow the Netsy defaults
// (client 2378, peer 2381, election 8443, health 8080) and are bound to the
// loopback interface for the single-node datastore.
const (
	DefaultBinary       = "netsy"
	DefaultClusterID    = "k8e"
	DefaultNodeID       = "k8e"
	DefaultClientPort   = 2378
	DefaultPeerPort     = 2381
	DefaultElectionPort = 8443
	DefaultHealthPort   = 8080
	// DefaultClientName is the identity carried in the `client` role URI SAN of
	// the certificate k8e uses to talk to the Netsy client API.
	DefaultClientName = "k8e"
)
