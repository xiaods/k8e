package rqlitecompat

import (
	"context"
	"crypto/tls"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"net"
	"time"

	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ServerVersion is the etcd wire version the layer reports. Clients gate
// features on it, so it is pinned to the etcd minor the compatibility matrix
// targets.
const ServerVersion = "3.7.1"

// DefaultMaxRecvMsgSize mirrors etcd's default --max-request-bytes (1.5 MiB).
const DefaultMaxRecvMsgSize = 1572864

// DefaultMaxSendMsgSize is the server-side send cap; a single watch batch or
// range chunk stays well below it.
const DefaultMaxSendMsgSize = 1 << 30

// maxRequestBytesOverhead is the room etcd gives the gRPC layer above
// MaxRequestBytes, so an oversized request is answered with the etcd error
// "etcdserver: request is too large" instead of a transport-level failure.
const maxRequestBytesOverhead = 512 * 1024

// Config configures a compatibility server.
type Config struct {
	// Endpoints are the rqlite HTTP endpoints the layer talks to.
	Endpoints []string
	// Owner identifies this server for lease bookkeeping.
	Owner string
	// MemberName and the URL lists are reported through Cluster.MemberList so
	// a client can discover the endpoints.
	MemberName string
	ClientURLs []string
	PeerURLs   []string
	// TLSConfig, when set, is used for the gRPC listener (TLS or mTLS).
	TLSConfig *tls.Config
	// MaxRecvMsgSize caps one inbound message. Zero uses the etcd default.
	MaxRecvMsgSize int
	// WatchPollInterval is how often a watch stream reads new events.
	WatchPollInterval time.Duration
	// LeaseExpiryInterval is how often expired leases are reaped.
	LeaseExpiryInterval time.Duration
	// DisableLeaseReaper stops the background expiry loop (tests that drive
	// expiry explicitly).
	DisableLeaseReaper bool
}

// Server is the etcd v3 gRPC surface backed by rqlite. The embedded
// Unimplemented* types supply mustEmbedUnimplemented* and make every RPC the
// layer does not implement fail with codes.Unimplemented instead of silently
// succeeding.
type Server struct {
	etcdserverpb.UnimplementedKVServer
	etcdserverpb.UnimplementedWatchServer
	etcdserverpb.UnimplementedLeaseServer
	etcdserverpb.UnimplementedMaintenanceServer
	etcdserverpb.UnimplementedClusterServer

	cfg    Config
	store  *Store
	grpc   *grpc.Server
	member uint64
	stop   chan struct{}
	cancel context.CancelFunc
}

// NewServer bootstraps the schema and builds the gRPC server. It does not
// listen yet; call Serve.
func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("rqlitecompat: at least one rqlite endpoint is required")
	}
	if cfg.Owner == "" {
		cfg.Owner = "k8e-rqlite-compat"
	}
	if cfg.MaxRecvMsgSize <= 0 {
		cfg.MaxRecvMsgSize = DefaultMaxRecvMsgSize
	}
	if cfg.WatchPollInterval <= 0 {
		cfg.WatchPollInterval = DefaultWatchPollInterval
	}
	client := NewClient(cfg.Endpoints...)
	if err := Bootstrap(ctx, client); err != nil {
		return nil, err
	}
	reaperCtx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:    cfg,
		store:  NewStore(client, cfg.Owner),
		member: memberID(cfg.MemberName),
		stop:   make(chan struct{}),
		cancel: cancel,
	}

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize + maxRequestBytesOverhead),
		grpc.MaxSendMsgSize(DefaultMaxSendMsgSize),
	}
	if cfg.TLSConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg.TLSConfig)))
	}
	s.grpc = grpc.NewServer(opts...)
	etcdserverpb.RegisterKVServer(s.grpc, s)
	etcdserverpb.RegisterWatchServer(s.grpc, s)
	etcdserverpb.RegisterLeaseServer(s.grpc, s)
	etcdserverpb.RegisterMaintenanceServer(s.grpc, s)
	etcdserverpb.RegisterClusterServer(s.grpc, s)

	if !cfg.DisableLeaseReaper {
		s.store.StartExpiry(reaperCtx, cfg.LeaseExpiryInterval)
	}
	return s, nil
}

// Store exposes the MVCC store (used by the harness and tests).
func (s *Server) Store() *Store { return s.store }

// MemberID is the id reported by Maintenance.Status and Cluster.MemberList.
func (s *Server) MemberID() uint64 { return s.member }

// Serve accepts connections until Stop is called.
func (s *Server) Serve(lis net.Listener) error {
	return s.grpc.Serve(lis)
}

// Stop gracefully stops the gRPC server and the lease reaper it owns.
func (s *Server) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
		s.cancel()
	}
	s.grpc.Stop()
}

func memberID(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("k8e-rqlite-compat"))
	if name != "" {
		_, _ = h.Write([]byte(name))
	}
	id := h.Sum64()
	if id == 0 {
		id = 1
	}
	return id
}

func (s *Server) header(rev int64) *etcdserverpb.ResponseHeader {
	if rev == 0 {
		if r, err := s.store.Revision(context.Background()); err == nil {
			rev = r
		}
	}
	return &etcdserverpb.ResponseHeader{ClusterId: s.member, MemberId: s.member, Revision: rev, RaftTerm: 1}
}

// checkRequestSize mirrors etcd's max-request-bytes enforcement: a write whose
// marshalled request exceeds the limit is rejected with
// "etcdserver: request is too large" rather than being applied.
func (s *Server) checkRequestSize(size int) error {
	if size > s.cfg.MaxRecvMsgSize {
		return rpctypes.ErrGRPCRequestTooLarge
	}
	return nil
}

// ---------------------------------------------------------------------------
// KV
// ---------------------------------------------------------------------------

// Range implements etcdserverpb.KVServer.
func (s *Server) Range(ctx context.Context, req *etcdserverpb.RangeRequest) (*etcdserverpb.RangeResponse, error) {
	if err := validateKey(req.Key); err != nil {
		return nil, err
	}
	result, err := s.store.Range(ctx, rangeOptions(req))
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.RangeResponse{Header: s.header(result.Revision), Count: result.Count, More: result.More}
	if !req.CountOnly {
		resp.Kvs = toPBKVs(result.KVs)
	}
	return resp, nil
}

// initialStreamChunkLimit is etcd's first chunk size for RangeStream; later
// chunks grow or shrink towards MaxRequestBytes.
const initialStreamChunkLimit = 10

// RangeStream implements etcdserverpb.KVServer. It streams the same result the
// unary Range returns, chunk by chunk, pinning the read revision for the whole
// stream so a write committed mid-stream cannot make the chunks inconsistent.
func (s *Server) RangeStream(req *etcdserverpb.RangeRequest, stream grpc.ServerStreamingServer[etcdserverpb.RangeStreamResponse]) error {
	if err := checkRangeStreamRequest(req); err != nil {
		return err
	}
	ctx := stream.Context()
	if req.CountOnly {
		result, err := s.store.Range(ctx, rangeOptions(req))
		if err != nil {
			return err
		}
		return stream.Send(&etcdserverpb.RangeStreamResponse{RangeResponse: &etcdserverpb.RangeResponse{
			Header: s.header(result.Revision),
			Count:  result.Count,
		}})
	}

	// The chunk loop walks the request forward itself, exactly like etcd's
	// rangeStream: each round reads a limit and starts after the previous
	// chunk's last key.
	totalLimit := req.Limit
	if totalLimit == 0 {
		totalLimit = math.MaxInt64
	}
	req.Limit = initialStreamChunkLimit
	if req.Limit > totalLimit {
		req.Limit = totalLimit
	}

	count := int64(0)
	headerRev := int64(0)
	for {
		result, err := s.store.Range(ctx, rangeOptions(req))
		if err != nil {
			return err
		}
		if headerRev == 0 {
			headerRev = result.Revision
			if req.Revision == 0 {
				req.Revision = headerRev
			}
		}
		count += int64(len(result.KVs))

		var nextKey []byte
		if result.More {
			nextKey = append(append([]byte{}, result.KVs[len(result.KVs)-1].Key...), 0x00)
		}
		out := &etcdserverpb.RangeResponse{Kvs: toPBKVs(result.KVs)}
		done := !result.More || count == totalLimit
		if done {
			out.Header = s.header(headerRev)
			out.More = result.More
			out.Count = count
			if result.More {
				remaining, err := s.store.Range(ctx, RangeOptions{
					Key: nextKey, RangeEnd: req.RangeEnd, Revision: req.Revision, CountOnly: true,
				})
				if err != nil {
					return err
				}
				out.Count += remaining.Count
			}
		}
		if err := stream.Send(&etcdserverpb.RangeStreamResponse{RangeResponse: out}); err != nil {
			return err
		}
		if done {
			return nil
		}
		req.Key = nextKey
		req.Limit = adjustChunkLimit(req.Limit, proto.Size(out), s.cfg.MaxRecvMsgSize)
		req.Limit = min(req.Limit, totalLimit-count)
	}
}

// checkRangeStreamRequest mirrors etcd: RangeStream serves the default key
// ordering only, and it rejects the revision filters the unary Range accepts.
func checkRangeStreamRequest(req *etcdserverpb.RangeRequest) error {
	if err := validateKey(req.Key); err != nil {
		return err
	}
	if order := req.SortOrder; order != etcdserverpb.RangeRequest_NONE &&
		!(req.SortTarget == etcdserverpb.RangeRequest_KEY && order == etcdserverpb.RangeRequest_ASCEND) {
		return status.Errorf(codes.Unimplemented, "RangeStream does not support custom sort orders")
	}
	if req.MinModRevision != 0 || req.MaxModRevision != 0 ||
		req.MinCreateRevision != 0 || req.MaxCreateRevision != 0 {
		return status.Errorf(codes.Unimplemented, "RangeStream does not support revision filters")
	}
	return nil
}

// adjustChunkLimit picks the next chunk's limit so each chunk lands near the
// request-size target; doubling/halving only outside [0.5x, 2x] avoids
// thrashing when a response sits near the boundary.
func adjustChunkLimit(lastLimit int64, lastSize, targetSize int) int64 {
	switch {
	case lastSize < targetSize/2:
		lastLimit *= 2
	case lastSize > targetSize*2:
		lastLimit /= 2
	}
	if lastLimit == 0 {
		lastLimit = 1
	}
	return lastLimit
}

// Put implements etcdserverpb.KVServer.
func (s *Server) Put(ctx context.Context, req *etcdserverpb.PutRequest) (*etcdserverpb.PutResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	if err := checkPutRequest(PutOptions{
		Key: req.Key, Value: req.Value, Lease: req.Lease,
		IgnoreValue: req.IgnoreValue, IgnoreLease: req.IgnoreLease,
	}); err != nil {
		return nil, err
	}
	result, err := s.store.Put(ctx, PutOptions{
		Key:         req.Key,
		Value:       req.Value,
		Lease:       req.Lease,
		PrevKv:      req.PrevKv,
		IgnoreValue: req.IgnoreValue,
		IgnoreLease: req.IgnoreLease,
	})
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.PutResponse{Header: s.header(result.Revision)}
	if req.PrevKv {
		resp.PrevKv = toPBKV(result.PrevKV)
	}
	return resp, nil
}

// DeleteRange implements etcdserverpb.KVServer.
func (s *Server) DeleteRange(ctx context.Context, req *etcdserverpb.DeleteRangeRequest) (*etcdserverpb.DeleteRangeResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	result, err := s.store.DeleteRange(ctx, DeleteOptions{Key: req.Key, RangeEnd: req.RangeEnd, PrevKv: req.PrevKv})
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.DeleteRangeResponse{Header: s.header(result.Revision), Deleted: result.Deleted}
	if req.PrevKv {
		resp.PrevKvs = toPBKVs(result.PrevKVs)
	}
	return resp, nil
}

// Compact implements etcdserverpb.KVServer.
func (s *Server) Compact(ctx context.Context, req *etcdserverpb.CompactionRequest) (*etcdserverpb.CompactionResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	if err := s.store.Compact(ctx, req.Revision, req.Physical); err != nil {
		return nil, err
	}
	rev, err := s.store.Revision(ctx)
	if err != nil {
		return nil, err
	}
	return &etcdserverpb.CompactionResponse{Header: s.header(rev)}, nil
}

// Txn implements etcdserverpb.KVServer.
func (s *Server) Txn(ctx context.Context, req *etcdserverpb.TxnRequest) (*etcdserverpb.TxnResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	txnReq, err := toTxnRequest(req)
	if err != nil {
		return nil, err
	}
	result, err := s.store.Txn(ctx, txnReq)
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.TxnResponse{Header: s.header(result.Revision), Succeeded: result.Succeeded}
	for _, r := range result.Responses {
		resp.Responses = append(resp.Responses, toPBResponseOp(r))
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Watch
// ---------------------------------------------------------------------------

// Watch implements etcdserverpb.WatchServer.
func (s *Server) Watch(stream grpc.BidiStreamingServer[etcdserverpb.WatchRequest, etcdserverpb.WatchResponse]) error {
	session := newWatchSession(s.store)
	ctx := stream.Context()

	recv := make(chan *etcdserverpb.WatchRequest)
	errc := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case recv <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(s.cfg.WatchPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case req := <-recv:
			if err := s.handleWatchRequest(ctx, session, stream, req); err != nil {
				return err
			}
		case <-ticker.C:
			batches, err := session.poll(ctx)
			if err != nil {
				return err
			}
			for _, b := range batches {
				if err := stream.Send(watchResponse(b)); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Server) handleWatchRequest(ctx context.Context, session *watchSession, stream grpc.BidiStreamingServer[etcdserverpb.WatchRequest, etcdserverpb.WatchResponse], req *etcdserverpb.WatchRequest) error {
	switch u := req.RequestUnion.(type) {
	case *etcdserverpb.WatchRequest_CreateRequest:
		c := u.CreateRequest
		batch, err := session.add(ctx, WatchCreate{
			ID:             c.WatchId,
			Key:            c.Key,
			RangeEnd:       c.RangeEnd,
			StartRevision:  c.StartRevision,
			ProgressNotify: c.ProgressNotify,
			PrevKV:         c.PrevKv,
			NoPut:          hasFilter(c.Filters, etcdserverpb.WatchCreateRequest_NOPUT),
			NoDelete:       hasFilter(c.Filters, etcdserverpb.WatchCreateRequest_NODELETE),
		})
		if err != nil {
			return err
		}
		return stream.Send(watchResponse(batch))
	case *etcdserverpb.WatchRequest_CancelRequest:
		session.cancel(u.CancelRequest.WatchId)
		return stream.Send(&etcdserverpb.WatchResponse{
			Header:   s.header(0),
			WatchId:  u.CancelRequest.WatchId,
			Canceled: true,
		})
	case *etcdserverpb.WatchRequest_ProgressRequest:
		rev, err := s.store.Revision(ctx)
		if err != nil {
			return err
		}
		return stream.Send(&etcdserverpb.WatchResponse{Header: s.header(rev)})
	default:
		return status.Error(codes.InvalidArgument, "etcdserver: unknown watch request")
	}
}

func watchResponse(b WatchBatch) *etcdserverpb.WatchResponse {
	resp := &etcdserverpb.WatchResponse{
		Header:          &etcdserverpb.ResponseHeader{Revision: b.Revision, RaftTerm: 1},
		WatchId:         b.WatchID,
		Created:         b.Created,
		Canceled:        b.Canceled,
		CompactRevision: b.CompactRevision,
	}
	for _, ev := range b.Events {
		pb := &mvccpb.Event{Kv: toPBKV(ev.KV)}
		if ev.Type == EventDelete {
			pb.Type = mvccpb.DELETE
		} else {
			pb.Type = mvccpb.PUT
		}
		if b.PrevKV && ev.PrevKV != nil {
			pb.PrevKv = toPBKV(ev.PrevKV)
		}
		resp.Events = append(resp.Events, pb)
	}
	return resp
}

func hasFilter(filters []etcdserverpb.WatchCreateRequest_FilterType, want etcdserverpb.WatchCreateRequest_FilterType) bool {
	for _, f := range filters {
		if f == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Lease
// ---------------------------------------------------------------------------

// LeaseGrant implements etcdserverpb.LeaseServer.
func (s *Server) LeaseGrant(ctx context.Context, req *etcdserverpb.LeaseGrantRequest) (*etcdserverpb.LeaseGrantResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	lease, err := s.store.LeaseGrant(ctx, req.ID, req.TTL)
	if err != nil {
		return nil, err
	}
	return &etcdserverpb.LeaseGrantResponse{Header: s.header(lease.Revision), ID: lease.ID, TTL: lease.TTL}, nil
}

// LeaseRevoke implements etcdserverpb.LeaseServer.
func (s *Server) LeaseRevoke(ctx context.Context, req *etcdserverpb.LeaseRevokeRequest) (*etcdserverpb.LeaseRevokeResponse, error) {
	if err := s.checkRequestSize(proto.Size(req)); err != nil {
		return nil, err
	}
	rev, err := s.store.LeaseRevoke(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return &etcdserverpb.LeaseRevokeResponse{Header: s.header(rev)}, nil
}

// LeaseKeepAlive implements etcdserverpb.LeaseServer.
func (s *Server) LeaseKeepAlive(stream grpc.BidiStreamingServer[etcdserverpb.LeaseKeepAliveRequest, etcdserverpb.LeaseKeepAliveResponse]) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ttl, err := s.store.LeaseKeepAlive(stream.Context(), req.ID)
		if err != nil {
			return err
		}
		rev, err := s.store.Revision(stream.Context())
		if err != nil {
			return err
		}
		if err := stream.Send(&etcdserverpb.LeaseKeepAliveResponse{Header: s.header(rev), ID: req.ID, TTL: ttl}); err != nil {
			return err
		}
	}
}

// LeaseTimeToLive implements etcdserverpb.LeaseServer.
func (s *Server) LeaseTimeToLive(ctx context.Context, req *etcdserverpb.LeaseTimeToLiveRequest) (*etcdserverpb.LeaseTimeToLiveResponse, error) {
	lease, kvs, err := s.store.LeaseTimeToLive(ctx, req.ID, req.Keys)
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.LeaseTimeToLiveResponse{
		Header:     s.header(0),
		ID:         lease.ID,
		TTL:        lease.Expiry,
		GrantedTTL: lease.TTL,
	}
	for _, kv := range kvs {
		resp.Keys = append(resp.Keys, kv.Key)
	}
	return resp, nil
}

// LeaseLeases implements etcdserverpb.LeaseServer.
func (s *Server) LeaseLeases(ctx context.Context, _ *etcdserverpb.LeaseLeasesRequest) (*etcdserverpb.LeaseLeasesResponse, error) {
	ids, err := s.store.LeaseLeases(ctx)
	if err != nil {
		return nil, err
	}
	resp := &etcdserverpb.LeaseLeasesResponse{Header: s.header(0)}
	for _, id := range ids {
		resp.Leases = append(resp.Leases, &etcdserverpb.LeaseStatus{ID: id})
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

// Status implements etcdserverpb.MaintenanceServer.
func (s *Server) Status(ctx context.Context, _ *etcdserverpb.StatusRequest) (*etcdserverpb.StatusResponse, error) {
	rev, err := s.store.Revision(ctx)
	if err != nil {
		return nil, err
	}
	return &etcdserverpb.StatusResponse{
		Header:         s.header(rev),
		Version:        ServerVersion,
		Leader:         s.member,
		RaftIndex:      uint64(rev),
		RaftTerm:       1,
		StorageVersion: ServerVersion,
	}, nil
}

// Alarm implements etcdserverpb.MaintenanceServer. Only the GET action is
// meaningful without alarms; activating one is rejected explicitly rather than
// silently ignored.
func (s *Server) Alarm(ctx context.Context, req *etcdserverpb.AlarmRequest) (*etcdserverpb.AlarmResponse, error) {
	if req.Action != etcdserverpb.AlarmRequest_GET {
		return nil, status.Error(codes.Unimplemented, "etcdserver: alarm activation is not supported by the rqlite compat layer")
	}
	rev, err := s.store.Revision(ctx)
	if err != nil {
		return nil, err
	}
	return &etcdserverpb.AlarmResponse{Header: s.header(rev)}, nil
}

// Defragment implements etcdserverpb.MaintenanceServer.
func (s *Server) Defragment(context.Context, *etcdserverpb.DefragmentRequest) (*etcdserverpb.DefragmentResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: defragment is owned by rqlite, not the compat layer")
}

// Hash implements etcdserverpb.MaintenanceServer.
func (s *Server) Hash(context.Context, *etcdserverpb.HashRequest) (*etcdserverpb.HashResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: backend hash is not available through the rqlite compat layer")
}

// HashKV implements etcdserverpb.MaintenanceServer.
func (s *Server) HashKV(context.Context, *etcdserverpb.HashKVRequest) (*etcdserverpb.HashKVResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: kv hash is not implemented by the rqlite compat layer")
}

// Snapshot implements etcdserverpb.MaintenanceServer.
func (s *Server) Snapshot(*etcdserverpb.SnapshotRequest, grpc.ServerStreamingServer[etcdserverpb.SnapshotResponse]) error {
	return status.Error(codes.Unimplemented, "etcdserver: snapshots are owned by rqlite, not the compat layer")
}

// MoveLeader implements etcdserverpb.MaintenanceServer.
func (s *Server) MoveLeader(context.Context, *etcdserverpb.MoveLeaderRequest) (*etcdserverpb.MoveLeaderResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: leadership is owned by rqlite, not the compat layer")
}

// Downgrade implements etcdserverpb.MaintenanceServer.
func (s *Server) Downgrade(context.Context, *etcdserverpb.DowngradeRequest) (*etcdserverpb.DowngradeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: version downgrade is not supported by the rqlite compat layer")
}

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------

// MemberList implements etcdserverpb.ClusterServer.
func (s *Server) MemberList(ctx context.Context, _ *etcdserverpb.MemberListRequest) (*etcdserverpb.MemberListResponse, error) {
	rev, err := s.store.Revision(ctx)
	if err != nil {
		return nil, err
	}
	name := s.cfg.MemberName
	if name == "" {
		name = "rqlite-compat"
	}
	return &etcdserverpb.MemberListResponse{
		Header: s.header(rev),
		Members: []*etcdserverpb.Member{{
			ID:         s.member,
			Name:       name,
			PeerURLs:   s.cfg.PeerURLs,
			ClientURLs: s.cfg.ClientURLs,
		}},
	}, nil
}

// MemberAdd implements etcdserverpb.ClusterServer.
func (s *Server) MemberAdd(context.Context, *etcdserverpb.MemberAddRequest) (*etcdserverpb.MemberAddResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: membership is owned by rqlite, not the compat layer")
}

// MemberRemove implements etcdserverpb.ClusterServer.
func (s *Server) MemberRemove(context.Context, *etcdserverpb.MemberRemoveRequest) (*etcdserverpb.MemberRemoveResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: membership is owned by rqlite, not the compat layer")
}

// MemberUpdate implements etcdserverpb.ClusterServer.
func (s *Server) MemberUpdate(context.Context, *etcdserverpb.MemberUpdateRequest) (*etcdserverpb.MemberUpdateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: membership is owned by rqlite, not the compat layer")
}

// MemberPromote implements etcdserverpb.ClusterServer.
func (s *Server) MemberPromote(context.Context, *etcdserverpb.MemberPromoteRequest) (*etcdserverpb.MemberPromoteResponse, error) {
	return nil, status.Error(codes.Unimplemented, "etcdserver: membership is owned by rqlite, not the compat layer")
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

func rangeOptions(req *etcdserverpb.RangeRequest) RangeOptions {
	return RangeOptions{
		Key:               req.Key,
		RangeEnd:          req.RangeEnd,
		Limit:             req.Limit,
		Revision:          req.Revision,
		SortOrder:         sortOrder(req.SortOrder),
		SortTarget:        sortTarget(req.SortTarget),
		KeysOnly:          req.KeysOnly,
		CountOnly:         req.CountOnly,
		MinModRevision:    req.MinModRevision,
		MaxModRevision:    req.MaxModRevision,
		MinCreateRevision: req.MinCreateRevision,
		MaxCreateRevision: req.MaxCreateRevision,
	}
}

func sortOrder(o etcdserverpb.RangeRequest_SortOrder) SortOrder {
	if o == etcdserverpb.RangeRequest_DESCEND {
		return SortDescend
	}
	return SortAscend
}

func sortTarget(t etcdserverpb.RangeRequest_SortTarget) SortTarget {
	switch t {
	case etcdserverpb.RangeRequest_VERSION:
		return SortByVersion
	case etcdserverpb.RangeRequest_CREATE:
		return SortByCreate
	case etcdserverpb.RangeRequest_MOD:
		return SortByMod
	case etcdserverpb.RangeRequest_VALUE:
		return SortByValue
	default:
		return SortByKey
	}
}

func toTxnRequest(req *etcdserverpb.TxnRequest) (TxnRequest, error) {
	out := TxnRequest{}
	for _, c := range req.Compare {
		cmp := Compare{Key: c.Key, RangeEnd: c.RangeEnd}
		switch c.Target {
		case etcdserverpb.Compare_VERSION:
			cmp.Target = CompareVersion
		case etcdserverpb.Compare_CREATE:
			cmp.Target = CompareCreate
		case etcdserverpb.Compare_MOD:
			cmp.Target = CompareMod
		case etcdserverpb.Compare_VALUE:
			cmp.Target = CompareValue
		case etcdserverpb.Compare_LEASE:
			cmp.Target = CompareLease
		}
		switch c.Result {
		case etcdserverpb.Compare_GREATER:
			cmp.Op = CompareGreater
		case etcdserverpb.Compare_LESS:
			cmp.Op = CompareLess
		case etcdserverpb.Compare_NOT_EQUAL:
			cmp.Op = CompareNotEqual
		default:
			cmp.Op = CompareEqual
		}
		switch u := c.TargetUnion.(type) {
		case *etcdserverpb.Compare_Version:
			cmp.Version = u.Version
		case *etcdserverpb.Compare_CreateRevision:
			cmp.Version = u.CreateRevision
		case *etcdserverpb.Compare_ModRevision:
			cmp.Version = u.ModRevision
		case *etcdserverpb.Compare_Lease:
			cmp.Version = u.Lease
		case *etcdserverpb.Compare_Value:
			cmp.Value = u.Value
		}
		out.Compares = append(out.Compares, cmp)
	}
	var err error
	if out.Success, err = toTxnOps(req.Success); err != nil {
		return TxnRequest{}, err
	}
	if out.Failure, err = toTxnOps(req.Failure); err != nil {
		return TxnRequest{}, err
	}
	return out, nil
}

func toTxnOps(ops []*etcdserverpb.RequestOp) ([]Op, error) {
	out := make([]Op, 0, len(ops))
	for _, op := range ops {
		switch u := op.Request.(type) {
		case *etcdserverpb.RequestOp_RequestRange:
			out = append(out, Op{Kind: OpRange, Range: ptr(rangeOptions(u.RequestRange))})
		case *etcdserverpb.RequestOp_RequestPut:
			out = append(out, Op{Kind: OpPut, Put: &PutOptions{
				Key:         u.RequestPut.Key,
				Value:       u.RequestPut.Value,
				Lease:       u.RequestPut.Lease,
				PrevKv:      u.RequestPut.PrevKv,
				IgnoreValue: u.RequestPut.IgnoreValue,
				IgnoreLease: u.RequestPut.IgnoreLease,
			}})
		case *etcdserverpb.RequestOp_RequestDeleteRange:
			out = append(out, Op{Kind: OpDelete, Delete: &DeleteOptions{
				Key:      u.RequestDeleteRange.Key,
				RangeEnd: u.RequestDeleteRange.RangeEnd,
				PrevKv:   u.RequestDeleteRange.PrevKv,
			}})
		case *etcdserverpb.RequestOp_RequestTxn:
			return nil, status.Error(codes.Unimplemented, "etcdserver: nested txn is not supported by the rqlite compat layer")
		default:
			return nil, rpctypes.ErrGRPCKeyNotFound
		}
	}
	return out, nil
}

func toPBResponseOp(r OpResponse) *etcdserverpb.ResponseOp {
	switch r.Kind {
	case OpRange:
		resp := &etcdserverpb.RangeResponse{Count: r.Range.Count, More: r.Range.More, Kvs: toPBKVs(r.Range.KVs)}
		return &etcdserverpb.ResponseOp{Response: &etcdserverpb.ResponseOp_ResponseRange{ResponseRange: resp}}
	case OpPut:
		resp := &etcdserverpb.PutResponse{}
		if r.Put.PrevKV != nil {
			resp.PrevKv = toPBKV(r.Put.PrevKV)
		}
		return &etcdserverpb.ResponseOp{Response: &etcdserverpb.ResponseOp_ResponsePut{ResponsePut: resp}}
	default:
		resp := &etcdserverpb.DeleteRangeResponse{Deleted: r.Delete.Deleted, PrevKvs: toPBKVs(r.Delete.PrevKVs)}
		return &etcdserverpb.ResponseOp{Response: &etcdserverpb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: resp}}
	}
}

func toPBKV(kv *KV) *mvccpb.KeyValue {
	if kv == nil {
		return nil
	}
	return &mvccpb.KeyValue{
		Key:            kv.Key,
		Value:          kv.Value,
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          kv.Lease,
	}
}

func toPBKVs(kvs []KV) []*mvccpb.KeyValue {
	out := make([]*mvccpb.KeyValue, 0, len(kvs))
	for i := range kvs {
		out = append(out, toPBKV(&kvs[i]))
	}
	return out
}

func ptr[T any](v T) *T { return &v }
