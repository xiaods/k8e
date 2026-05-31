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
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

const snapshotsDir = ".k8e/sandbox/snapshots"

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
	home, _ := os.UserHomeDir()
	return filepath.Join(home, snapshotsDir, name)
}

func snapshotMetaPath(name string) string {
	return filepath.Join(snapshotDir(name), "metadata.json")
}

func snapshotTarPath(name string) string {
	return filepath.Join(snapshotDir(name), "workspace.tar.gz")
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
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().Get(0)
			name := ctx.Args().Get(1)
			if sid == "" || name == "" {
				printErrorExit("usage: k8e-sandbox-cli snapshot save <session-id> <name>", 1)
				return nil
			}

			client := newClientFromCtx(ctx)
			defer client.Close()

			// 1. Create tar.gz inside sandbox
			fmt.Fprintf(os.Stderr, "[k8e-sandbox] archiving workspace...\n")
			_, err := client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
				SessionId: sid,
				Command:   "tar czf /tmp/_snapshot.tar.gz -C /workspace . 2>/dev/null",
				Timeout:   300,
			})
			if err != nil {
				printErrorExit("archive workspace: "+err.Error(), 1)
				return nil
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
				printErrorExit("read snapshot: "+err.Error(), 1)
				return nil
			}

			// 4. Save to disk
			dir := snapshotDir(name)
			if err := os.MkdirAll(dir, 0755); err != nil {
				printErrorExit("create snapshot dir: "+err.Error(), 2)
				return nil
			}
			if err := os.WriteFile(snapshotTarPath(name), []byte(readResp.Content), 0644); err != nil {
				printErrorExit("write snapshot: "+err.Error(), 2)
				return nil
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
		Usage: "List saved snapshots",
		Action: func(ctx *cli.Context) error {
			home, _ := os.UserHomeDir()
			base := filepath.Join(home, snapshotsDir)
			entries, err := os.ReadDir(base)
			if err != nil {
				if os.IsNotExist(err) {
					printJSON(map[string]any{"snapshots": []any{}})
					return nil
				}
				printErrorExit("read snapshots dir: "+err.Error(), 2)
				return nil
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
				printErrorExit("usage: k8e-sandbox-cli snapshot restore <name>", 1)
				return nil
			}

			// 1. Read metadata
			metaData, err := os.ReadFile(snapshotMetaPath(name))
			if err != nil {
				printErrorExit("snapshot not found: "+name, 1)
				return nil
			}
			var meta SnapshotMeta
			if json.Unmarshal(metaData, &meta) != nil {
				printErrorExit("snapshot metadata corrupted: "+name, 2)
				return nil
			}

			// 2. Read tar.gz
			tarData, err := os.ReadFile(snapshotTarPath(name))
			if err != nil {
				printErrorExit("snapshot data not found: "+name, 2)
				return nil
			}

			// 3. Create new session
			client := newClientFromCtx(ctx)
			defer client.Close()

			var hosts []string
			if raw := ctx.String("allowed-hosts"); raw != "" {
				hosts = strings.Split(raw, ",")
			}

			resp, err := client.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
				TenantId:     ctx.String("tenant"),
				RuntimeClass: ctx.String("runtime"),
				AllowedHosts: hosts,
			})
			if err != nil {
				printErrorExit("create session: "+err.Error(), 2)
				return nil
			}

			sid := resp.SessionId

			// 4. Upload tar.gz to sandbox
			// Use WriteFile for large data (handled by gRPC streaming)
			_, err = client.SandboxServiceClient.WriteFile(context.Background(), &pb.WriteFileRequest{
				SessionId: sid, Path: "/tmp/_snapshot.tar.gz", Content: string(tarData), Mode: "w",
			})
			if err != nil {
				client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
				printErrorExit("upload snapshot: "+err.Error(), 2)
				return nil
			}

			// 5. Extract
			_, err = client.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
				SessionId: sid,
				Command:   "tar xzf /tmp/_snapshot.tar.gz -C /workspace && rm -f /tmp/_snapshot.tar.gz",
				Timeout:   120,
			})
			if err != nil {
				client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
				printErrorExit("extract snapshot: "+err.Error(), 1)
				return nil
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
				printErrorExit("usage: k8e-sandbox-cli snapshot delete <name>", 1)
				return nil
			}

			dir := snapshotDir(name)
			if err := os.RemoveAll(dir); err != nil {
				printErrorExit("delete snapshot: "+err.Error(), 2)
				return nil
			}
			printJSON(map[string]any{"ok": true})
			return nil
		},
	}
}
