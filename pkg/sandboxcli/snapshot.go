package sandboxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli"
	sandboxclient "github.com/xiaods/k8e/pkg/sandbox/client"
	"github.com/xiaods/k8e/pkg/sandboxlayer"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

const snapshotDirName = "snapshots"

// SnapshotMeta holds metadata about a saved snapshot.
type SnapshotMeta struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	SizeBytes int64  `json:"size_bytes"`
	FileCount int    `json:"file_count"`
}

func snapshotDir(name string) string {
	dir, _ := dataDir()
	return filepath.Join(dir, snapshotDirName, name)
}

func snapshotMetaPath(name string) string {
	return filepath.Join(snapshotDir(name), "metadata.json")
}

func snapshotTarPath(name string) string {
	return filepath.Join(snapshotDir(name), "workspace.tar.gz")
}

// snapshotStore opens the content-addressed layer store backing snapshots
// (KIP-16 M2 / issue #511). Layers are stored under <dataDir>/layers and are
// deduplicated by sha256; each snapshot's manifest leases its layers from GC.
func snapshotStore() (*sandboxlayer.Store, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, err
	}
	return sandboxlayer.New(filepath.Join(dir, snapshotDirName, ".layers"))
}

// storeSnapshotLayer content-addresses a snapshot payload into the layer store
// and leases it via a manifest (KIP-16 M2: dedup + GC lease).
func storeSnapshotLayer(name string, content []byte) error {
	store, err := snapshotStore()
	if err != nil {
		return fmt.Errorf("open layer store: %w", err)
	}
	digest, err := store.Put(content)
	if err != nil {
		return fmt.Errorf("store snapshot layer: %w", err)
	}
	if err := store.SaveManifest(name, []string{digest}); err != nil {
		return fmt.Errorf("store snapshot manifest: %w", err)
	}
	return nil
}

// publishSnapshotRemote publishes a snapshot's manifest (layer list) to the
// gateway's server-side layer registry (KIP-16 M2 / issue #511). The gateway
// returns FailedPrecondition when the registry is not enabled server-side.
func publishSnapshotRemote(client *sandboxclient.Client, name string) error {
	store, err := snapshotStore()
	if err != nil {
		return err
	}
	m, err := store.LoadManifest(name)
	if err != nil {
		return fmt.Errorf("local manifest for %s: %w", name, err)
	}
	_, err = client.SandboxServiceClient.SnapshotPut(context.Background(), &pb.SnapshotPutRequest{
		Name:   name,
		Layers: m.Layers,
	})
	return err
}

// splitAllowedHosts splits a comma-separated egress allowlist, returning nil
// for an empty input.
func splitAllowedHosts(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// readSnapshotMeta loads and validates a snapshot's metadata file.
func readSnapshotMeta(name string) (SnapshotMeta, error) {
	var meta SnapshotMeta
	data, err := os.ReadFile(snapshotMetaPath(name))
	if err != nil {
		return meta, fmt.Errorf("snapshot not found: %s", name)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("snapshot metadata corrupted: %s", name)
	}
	return meta, nil
}

// readSnapshotPayload loads a snapshot's payload bytes: from the content-
// addressed layer store when present, otherwise from the legacy tar file.
func readSnapshotPayload(name string) ([]byte, error) {
	if store, err := snapshotStore(); err == nil {
		if m, err := store.LoadManifest(name); err == nil && len(m.Layers) == 1 {
			if content, err := store.Get(m.Layers[0]); err == nil {
				return content, nil
			}
		}
	}
	return os.ReadFile(snapshotTarPath(name))
}

// uploadAndExtractSnapshot uploads the payload to the session sandbox and
// extracts it into /workspace, removing the temp file afterwards.
func uploadAndExtractSnapshot(client *sandboxclient.Client, sid string, payload []byte) error {
	if _, err := client.SandboxServiceClient.WriteFile(context.Background(), &pb.WriteFileRequest{
		SessionId: sid, Path: "/tmp/_snapshot.tar.gz", Content: string(payload), Mode: "w",
	}); err != nil {
		return fmt.Errorf("upload snapshot: %w", err)
	}
	if _, err := client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
		SessionId: sid,
		Command:   "tar xzf /tmp/_snapshot.tar.gz -C /workspace && rm -f /tmp/_snapshot.tar.gz",
		Timeout:   120,
	}); err != nil {
		return fmt.Errorf("extract snapshot: %w", err)
	}
	return nil
}

// ── SnapshotCommand ─────────────────────────────────────────────────────────

func SnapshotCommand() cli.Command {
	return cli.Command{
		Name:  "snapshot",
		Usage: "Save and restore sandbox workspace snapshots",
		Subcommands: []cli.Command{
			snapshotSaveCommand(),
			snapshotListCommand(),
			snapshotRestoreCommand(),
			snapshotDeleteCommand(),
		},
	}
}

func snapshotSaveCommand() cli.Command {
	return cli.Command{
		Name:      "save",
		Usage:     "Save the workspace of a session as a named snapshot",
		ArgsUsage: "<session-id> <name>",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "remote", Usage: "Also publish the manifest to the gateway's server-side layer registry (KIP-16 M2)"},
		},
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().Get(0)
			name := ctx.Args().Get(1)
			if sid == "" || name == "" {
				return printErrorExit("usage: k8e-sandbox-cli snapshot save <session-id> <name>", 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			// 1. Create tar.gz inside sandbox
			fmt.Fprintf(os.Stderr, "[k8e-sandbox] archiving workspace...\n")
			_, err := client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
				SessionId: sid,
				Command:   "tar czf /tmp/_snapshot.tar.gz -C /workspace . 2>/dev/null",
				Timeout:   300,
			})
			if err != nil {
				return printErrorExit("archive workspace: "+err.Error(), 1)
			}

			// 2. Get file count and size
			countResp, err := client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
				SessionId: sid,
				Command:   "find /workspace -type f | wc -l",
				Timeout:   30,
			})
			fileCount := 0
			if err == nil {
				fmt.Sscanf(countResp.Stdout, "%d", &fileCount)
			}

			// 3. Download tar.gz
			fmt.Fprintf(os.Stderr, "[k8e-sandbox] downloading snapshot...\n")
			readResp, err := client.SandboxServiceClient.ReadFile(context.Background(), &pb.ReadFileRequest{
				SessionId: sid, Path: "/tmp/_snapshot.tar.gz",
			})
			if err != nil {
				return printErrorExit("read snapshot: "+err.Error(), 1)
			}

			// 4. Save to disk
			dir := snapshotDir(name)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return printErrorExit("create snapshot dir: "+err.Error(), 2)
			}
			if err := os.WriteFile(snapshotTarPath(name), []byte(readResp.Content), 0644); err != nil {
				return printErrorExit("write snapshot: "+err.Error(), 2)
			}

			// 4b. Content-address the payload into the layer store and lease it
			// via a manifest (KIP-16 M2: dedup + GC lease).
			if err := storeSnapshotLayer(name, []byte(readResp.Content)); err != nil {
				return printErrorExit(err.Error(), 2)
			}

			// 4c. Optionally publish to the gateway's server-side layer registry.
			if ctx.Bool("remote") {
				if err := publishSnapshotRemote(client, name); err != nil {
					return printErrorExit("publish remote: "+err.Error(), 2)
				}
				fmt.Fprintf(os.Stderr, "[k8e-sandbox] published snapshot %s to gateway registry\n", name)
			}

			// 5. Write metadata
			meta := SnapshotMeta{
				Name:      name,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				SessionID: sid,
				SizeBytes: int64(len(readResp.Content)),
				FileCount: fileCount,
			}
			metaData, _ := json.MarshalIndent(meta, "", "  ")
			os.WriteFile(snapshotMetaPath(name), metaData, 0644) //nolint:errcheck

			// 6. Cleanup temp file in sandbox
			client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
				SessionId: sid, Command: "rm -f /tmp/_snapshot.tar.gz", Timeout: 10,
			}) //nolint:errcheck

			printJSON(map[string]any{
				"ok": true, "name": name,
				"size_bytes": meta.SizeBytes, "file_count": meta.FileCount,
			})
			return nil
		},
	}
}

func snapshotListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "List saved snapshots (--remote: gateway registry)",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "remote", Usage: "List from the gateway's server-side layer registry"},
		},
		Action: func(ctx *cli.Context) error {
			if ctx.Bool("remote") {
				return snapshotListRemote(ctx)
			}
			dir, _ := dataDir()
			base := filepath.Join(dir, snapshotDirName)
			entries, err := os.ReadDir(base)
			if err != nil {
				if os.IsNotExist(err) {
					printJSON(map[string]any{"snapshots": []any{}})
					return nil
				}
				return printErrorExit("read snapshots dir: "+err.Error(), 2)
			}

			var snapshots []map[string]any
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				data, err := os.ReadFile(snapshotMetaPath(e.Name()))
				if err != nil {
					continue
				}
				var meta SnapshotMeta
				if json.Unmarshal(data, &meta) != nil {
					continue
				}
				snapshots = append(snapshots, map[string]any{
					"name": meta.Name, "created_at": meta.CreatedAt,
					"size_bytes": meta.SizeBytes, "file_count": meta.FileCount,
				})
			}
			printJSON(map[string]any{"snapshots": snapshots})
			return nil
		},
	}
}

// snapshotListRemote lists snapshot manifests from the gateway's server-side
// layer registry (KIP-16 M2 / issue #511).
func snapshotListRemote(ctx *cli.Context) error {
	client, exitErr := newClientFromCtx(ctx)
	if exitErr != nil {
		return exitErr
	}
	defer client.Close()
	resp, err := client.SandboxServiceClient.SnapshotList(context.Background(), &pb.SnapshotListRequest{})
	if err != nil {
		return printErrorExit("snapshot list --remote: "+err.Error(), 1)
	}
	printJSON(map[string]any{"snapshots": resp.Names})
	return nil
}

func snapshotRestoreCommand() cli.Command {
	return cli.Command{
		Name:      "restore",
		Usage:     "Create a new session from a saved snapshot",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "runtime", Value: "gvisor", Usage: "Runtime class"},
			cli.StringFlag{Name: "tenant", EnvVar: "K8E_SANDBOX_TENANT", Usage: "Tenant identifier"},
			cli.StringFlag{Name: "allowed-hosts", Usage: "Comma-separated FQDN egress allowlist"},
		},
		Action: func(ctx *cli.Context) error {
			name := ctx.Args().First()
			if name == "" {
				return printErrorExit("usage: k8e-sandbox-cli snapshot restore <name>", 1)
			}

			// 1. Read metadata
			meta, err := readSnapshotMeta(name)
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}

			// 2. Read snapshot payload. Prefer the content-addressed layer store
			// (KIP-16 M2); fall back to the legacy tar file for old snapshots.
			tarData, err := readSnapshotPayload(name)
			if err != nil {
				return printErrorExit("snapshot data not found: "+name, 2)
			}

			// 3. Create new session
			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
				TenantId:     ctx.String("tenant"),
				RuntimeClass: ctx.String("runtime"),
				AllowedHosts: splitAllowedHosts(ctx.String("allowed-hosts")),
			})
			if err != nil {
				return printErrorExit("create session: "+err.Error(), 2)
			}

			sid := resp.SessionId

			// 4-5. Upload + extract snapshot payload.
			if err := uploadAndExtractSnapshot(client, sid, tarData); err != nil {
				client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid}) //nolint:errcheck
				return printErrorExit(err.Error(), 2)
			}

			// 6. Write state (inherit tenant from snapshot if not overridden)
			tenant := ctx.String("tenant")
			if tenant == "" {
				tenant = meta.TenantID
			}
			_ = finalizeState(tenant, sid)

			printJSON(map[string]any{
				"session_id": sid, "pod_ip": resp.PodIp, "tenant_id": tenant, "restored_from": name,
			})
			return nil
		},
	}
}

func snapshotDeleteCommand() cli.Command {
	return cli.Command{
		Name:      "delete",
		Usage:     "Delete a saved snapshot",
		ArgsUsage: "<name>",
		Action: func(ctx *cli.Context) error {
			name := ctx.Args().First()
			if name == "" {
				return printErrorExit("usage: k8e-sandbox-cli snapshot delete <name>", 1)
			}

			dir := snapshotDir(name)
			if err := os.RemoveAll(dir); err != nil {
				return printErrorExit("delete snapshot: "+err.Error(), 2)
			}

			// Release the manifest lease so GC can reclaim the layers
			// (KIP-16 M2 lease-driven GC).
			if store, err := snapshotStore(); err == nil {
				_ = store.DeleteManifest(name)
				_, _ = store.GC()
			}
			printJSON(map[string]any{"ok": true})
			return nil
		},
	}
}
