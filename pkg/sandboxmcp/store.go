package sandboxmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

var ErrRecordMissing = errors.New("MCP record not found")
var ErrRecordExists = errors.New("MCP record already exists")

type Record struct {
	Owner     string          `json:"owner"`
	Digest    string          `json:"digest,omitempty"`
	State     string          `json:"state"`
	SessionID string          `json:"session_id,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Version   string          `json:"-"`
}

type RecordStore interface {
	Create(context.Context, string, Record) (Record, error)
	Get(context.Context, string) (Record, error)
	Update(context.Context, string, Record) error
}

type KubernetesStore struct{ maps typedcore.ConfigMapInterface }

func NewKubernetesStore(maps typedcore.ConfigMapInterface) (*KubernetesStore, error) {
	if maps == nil {
		return nil, errors.New("ConfigMap client required")
	}
	return &KubernetesStore{maps: maps}, nil
}

func recordKey(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return "mcp-" + hex.EncodeToString(digest[:28])
}

func encodeRecord(key string, record Record) (*corev1.ConfigMap, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(data) > 512*1024 {
		return nil, errors.New("MCP record exceeds size limit")
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key, ResourceVersion: record.Version, Labels: map[string]string{"k8e.sh/mcp-state": "true"}}, Data: map[string]string{"record": string(data)}}, nil
}

func decodeRecord(configMap *corev1.ConfigMap) (Record, error) {
	var record Record
	if err := json.Unmarshal([]byte(configMap.Data["record"]), &record); err != nil {
		return record, fmt.Errorf("invalid MCP state: %w", err)
	}
	if record.Owner == "" || record.State == "" {
		return record, errors.New("incomplete MCP state")
	}
	record.Version = configMap.ResourceVersion
	return record, nil
}

func (store *KubernetesStore) Create(ctx context.Context, key string, record Record) (Record, error) {
	configMap, err := encodeRecord(key, record)
	if err != nil {
		return Record{}, err
	}
	created, err := store.maps.Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return Record{}, ErrRecordExists
	}
	if err != nil {
		return Record{}, err
	}
	return decodeRecord(created)
}

func (store *KubernetesStore) Get(ctx context.Context, key string) (Record, error) {
	configMap, err := store.maps.Get(ctx, key, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Record{}, ErrRecordMissing
	}
	if err != nil {
		return Record{}, err
	}
	return decodeRecord(configMap)
}

func (store *KubernetesStore) Update(ctx context.Context, key string, record Record) error {
	if record.Version == "" {
		return errors.New("record version required")
	}
	configMap, err := encodeRecord(key, record)
	if err != nil {
		return err
	}
	_, err = store.maps.Update(ctx, configMap, metav1.UpdateOptions{})
	return err
}
