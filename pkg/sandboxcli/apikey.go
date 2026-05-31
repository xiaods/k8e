package sandboxcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const apiKeySecretName = "sandbox-api-keys"
const apiKeySecretNS = "sandbox-matrix"

type apiKeyStore map[string]string // name → key

func readAPIKeys() (apiKeyStore, error) {
	k8s, err := newK8sClient()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig required for api-key management: %w", err)
	}
	secret, err := k8s.CoreV1().Secrets(apiKeySecretNS).Get(context.Background(), apiKeySecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("api-key secret not found (run 'k8e sandbox-api-key create <name>' to create your first key)")
	}
	data, ok := secret.Data["keys.json"]
	if !ok {
		return nil, fmt.Errorf("api-key secret corrupted: missing keys.json")
	}
	var store apiKeyStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("api-key secret corrupted: %w", err)
	}
	if store == nil {
		store = make(apiKeyStore)
	}
	return store, nil
}

func writeAPIKeys(store apiKeyStore) error {
	k8s, err := newK8sClient()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(store)
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

func newK8sClient() (kubernetes.Interface, error) {
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
	return kubernetes.NewForConfig(cfg)
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
		Usage:     "Create a new API key",
		ArgsUsage: "<name>",
		Action: func(ctx *cli.Context) error {
			name := ctx.Args().First()
			if name == "" {
				printErrorExit("usage: k8e sandbox-api-key create <name>", 1)
				return nil
			}
			store, err := readAPIKeys()
			if err != nil {
				printErrorExit(err.Error(), 1)
				return nil
			}
			if _, exists := store[name]; exists {
				printErrorExit("api-key '"+name+"' already exists", 1)
				return nil
			}
			key := generateAPIKey()
			store[name] = key
			if err := writeAPIKeys(store); err != nil {
				printErrorExit("write api-key: "+err.Error(), 2)
				return nil
			}
			printJSON(map[string]any{"name": name, "key": key})
			return nil
		},
	}
}

func apiKeyListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "List API keys (names only, keys are not shown)",
		Action: func(ctx *cli.Context) error {
			store, err := readAPIKeys()
			if err != nil {
				printErrorExit(err.Error(), 1)
				return nil
			}
			names := make([]string, 0, len(store))
			for k := range store {
				names = append(names, k)
			}
			printJSON(map[string]any{"keys": names})
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
				printErrorExit("usage: k8e sandbox-api-key delete <name>", 1)
				return nil
			}
			store, err := readAPIKeys()
			if err != nil {
				printErrorExit(err.Error(), 1)
				return nil
			}
			if _, exists := store[name]; !exists {
				printErrorExit("api-key '"+name+"' not found", 1)
				return nil
			}
			delete(store, name)
			if err := writeAPIKeys(store); err != nil {
				printErrorExit("write api-key: "+err.Error(), 2)
				return nil
			}
			printJSON(map[string]any{"ok": true, "name": name})
			return nil
		},
	}
}
