package sandboxcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConnectionConfig is persisted by `connect` so later CLI invocations
// can dial the same local or remote gateway without repeating flags.
type ConnectionConfig struct {
	// Mode is "local" or "remote".
	Mode string `json:"mode"`
	// Endpoint is the gRPC address (host:port). Empty means default local.
	Endpoint string `json:"endpoint,omitempty"`
	// Agents records which agent harnesses received the skill on last connect.
	Agents []string `json:"agents,omitempty"`
}

const connectionConfigFile = "config.json"

func connectionConfigPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, connectionConfigFile), nil
}

// LoadConnectionConfig reads ~/.k8e/sandbox/config.json. Returns nil, nil when absent.
func LoadConnectionConfig() (*ConnectionConfig, error) {
	path, err := connectionConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read connection config: %w", err)
	}
	var cfg ConnectionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse connection config: %w", err)
	}
	return &cfg, nil
}

// SaveConnectionConfig writes connection config with mode 0600.
func SaveConnectionConfig(cfg *ConnectionConfig) error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create sandbox data dir: %w", err)
	}
	path := filepath.Join(dir, connectionConfigFile)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write connection config: %w", err)
	}
	return nil
}
