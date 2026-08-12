package sandboxcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandbox/apikey"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const apiKeySecretName = "sandbox-apikeys"
const apiKeySecretNS = "sandbox-matrix"

func readAPIKeys() (map[string]apikey.Record, error) {
	k8s, err := newK8sClient()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig required for api-key management: %w", err)
	}
	secret, err := k8s.CoreV1().Secrets(apiKeySecretNS).Get(context.Background(), apiKeySecretName, metav1.GetOptions{})
	if err != nil {
		return map[string]apikey.Record{}, nil // secret not created yet — first key
	}
	data, ok := secret.Data["keys.json"]
	if !ok {
		return nil, fmt.Errorf("api-key secret corrupted: missing keys.json")
	}
	store, err := apikey.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("api-key secret corrupted: %w", err)
	}
	if store == nil {
		store = map[string]apikey.Record{}
	}
	return store, nil
}

func writeAPIKeys(store map[string]apikey.Record) error {
	k8s, err := newK8sClient()
	if err != nil {
		return err
	}
	data, err := apikey.Encode(store)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: apiKeySecretName, Namespace: apiKeySecretNS},
		Data:       map[string][]byte{"keys.json": data},
	}
	// try update first, create if not found
	_, err = k8s.CoreV1().Secrets(apiKeySecretNS).Update(context.Background(), secret, metav1.UpdateOptions{})
	if err != nil {
		_, err = k8s.CoreV1().Secrets(apiKeySecretNS).Create(context.Background(), secret, metav1.CreateOptions{})
	}
	return err
}

func generateAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b) //nolint:errcheck
	return "k8e-" + hex.EncodeToString(b)
}

var cachedK8sClient kubernetes.Interface

func newK8sClient() (kubernetes.Interface, error) {
	if cachedK8sClient != nil {
		return cachedK8sClient, nil
	}
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			kc = home + "/.kube/config"
		}
	}
	if kc == "" {
		kc = "/etc/k8e/k8e.yaml"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kc)
	if err != nil {
		return nil, err
	}
	cachedK8sClient, err = kubernetes.NewForConfig(cfg)
	return cachedK8sClient, err
}

// ── ApiKeyCommand ──────────────────────────────────────────────────────────

func ApiKeyCommand() cli.Command {
	return cli.Command{
		Name:  "api-key",
		Usage: "Manage sandbox API keys for remote cluster access",
		Subcommands: []cli.Command{
			apiKeyCreateCommand(),
			apiKeyListCommand(),
			apiKeyDeleteCommand(),
		},
	}
}

func apiKeyCreateCommand() cli.Command {
	return cli.Command{
		Name:      "create",
		Usage:     "Create a new API key (default TTL 30 days)",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "ttl",
				Value: "30d",
				Usage: "Key lifetime: 30d (default), 90d, 720h, or never",
			},
		},
		Action: func(ctx *cli.Context) error {
			name := ctx.Args().First()
			if name == "" {
				return printErrorExit("usage: k8e sandbox-apikey create <name> [--ttl 30d|never]", 1)
			}
			ttlDays, never, err := apikey.ParseTTL(ctx.String("ttl"))
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}
			store, err := readAPIKeys()
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}
			if _, exists := store[name]; exists {
				return printErrorExit("api-key '"+name+"' already exists", 1)
			}
			key := generateAPIKey()
			rec := apikey.NewRecord(key, ttlDays, never, time.Now())
			store[name] = rec
			if err := writeAPIKeys(store); err != nil {
				return printErrorExit("write api-key: "+err.Error(), 2)
			}
			out := map[string]any{
				"name":       name,
				"key":        key,
				"ttl_days":   rec.TTLDays,
				"created_at": rec.CreatedAt.UTC().Format(time.RFC3339),
			}
			if rec.ExpiresAt != nil {
				out["expires_at"] = rec.ExpiresAt.UTC().Format(time.RFC3339)
			} else {
				out["expires_at"] = nil
				out["ttl_days"] = nil
			}
			printJSON(out)
			return nil
		},
	}
}

func apiKeyListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "List API keys (names + expiry; secrets are not shown)",
		Action: func(ctx *cli.Context) error {
			store, err := readAPIKeys()
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}
			now := time.Now()
			items := make([]map[string]any, 0, len(store))
			for name, rec := range store {
				item := map[string]any{
					"name":    name,
					"expired": rec.Expired(now),
				}
				if !rec.CreatedAt.IsZero() {
					item["created_at"] = rec.CreatedAt.UTC().Format(time.RFC3339)
				}
				if rec.ExpiresAt != nil {
					item["expires_at"] = rec.ExpiresAt.UTC().Format(time.RFC3339)
					item["ttl_days"] = rec.TTLDays
				}
				items = append(items, item)
			}
			printJSON(map[string]any{"keys": items})
			return nil
		},
	}
}

func apiKeyDeleteCommand() cli.Command {
	return cli.Command{
		Name:      "delete",
		Usage:     "Delete an API key",
		ArgsUsage: "<name>",
		Action: func(ctx *cli.Context) error {
			name := ctx.Args().First()
			if name == "" {
				return printErrorExit("usage: k8e sandbox-apikey delete <name>", 1)
			}
			store, err := readAPIKeys()
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}
			if _, exists := store[name]; !exists {
				return printErrorExit("api-key '"+name+"' not found", 1)
			}
			delete(store, name)
			if err := writeAPIKeys(store); err != nil {
				return printErrorExit("write api-key: "+err.Error(), 2)
			}
			printJSON(map[string]any{"ok": true, "name": name})
			return nil
		},
	}
}
