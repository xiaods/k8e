package sandboxmcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Service exposes sandbox gRPC operations as authenticated MCP tools.
type Service struct {
	backend pb.SandboxServiceClient
	store   RecordStore
}

type arguments struct {
	OperationID   string `json:"operation_id,omitempty"`
	OperationKind string `json:"operation_kind,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	Command       string `json:"command,omitempty"`
	Path          string `json:"path,omitempty"`
	Content       string `json:"content,omitempty"`
	Timeout       int32  `json:"timeout,omitempty"`
}

// NewService creates a tool service backed by the sandbox gateway and durable state.
func NewService(backend pb.SandboxServiceClient, store RecordStore) (*Service, error) {
	if backend == nil || store == nil {
		return nil, errors.New("backend and durable store required")
	}
	return &Service{backend: backend, store: store}, nil
}

// Tools returns the sandbox tool definitions bound to this service.
func (service *Service) Tools() []Tool {
	definitions := []struct {
		name, description string
		fields, required  []string
	}{
		{"sandbox_create", "Create a gVisor sandbox; reuse operation_id on retries.", []string{"operation_id"}, []string{"operation_id"}},
		{"sandbox_get", "Get an owned sandbox.", []string{"session_id"}, []string{"session_id"}},
		{"sandbox_destroy", "Destroy an owned sandbox.", []string{"session_id", "operation_id"}, []string{"session_id", "operation_id"}},
		{"sandbox_exec", "Submit a background shell command. Poll run_id; reuse operation_id on retries.", []string{"session_id", "operation_id", "command", "timeout"}, []string{"session_id", "operation_id", "command"}},
		{"sandbox_poll", "Poll an owned background run.", []string{"run_id"}, []string{"run_id"}},
		{"sandbox_read", "Read at most 256 KiB of an owned sandbox file as base64.", []string{"session_id", "path"}, []string{"session_id", "path"}},
		{"sandbox_write", "Write UTF-8 text to an owned sandbox file.", []string{"session_id", "operation_id", "path", "content"}, []string{"session_id", "operation_id", "path", "content"}},
		{"sandbox_list", "List files in an owned sandbox.", []string{"session_id"}, []string{"session_id"}},
		{"sandbox_operation", "Recover a submission result using its operation_id and tool name as operation_kind. Unknown submissions are never automatically resubmitted.", []string{"operation_id", "operation_kind"}, []string{"operation_id", "operation_kind"}},
	}
	var tools []Tool
	for _, definition := range definitions {
		properties := map[string]any{}
		for _, field := range definition.fields {
			limit := 256
			if field == "command" || field == "content" {
				limit = 256 * 1024
			}
			minimum := 1
			if field == "content" {
				minimum = 0
			}
			properties[field] = map[string]any{"type": "string", "minLength": minimum, "maxLength": limit}
			if field == "timeout" {
				properties[field] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "default": 30}
			}
		}
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": definition.required, "additionalProperties": false})
		name, fields, required := definition.name, definition.fields, definition.required
		tools = append(tools, Tool{Name: name, Description: definition.description, InputSchema: schema, Call: func(ctx context.Context, principal Principal, raw json.RawMessage) (CallResult, error) {
			parsed, err := parseArguments(raw, fields, required)
			if err != nil {
				return CallResult{}, err
			}
			return service.call(ctx, principal, name, parsed)
		}})
	}
	return tools
}

func parseArguments(raw json.RawMessage, fields, required []string) (arguments, error) {
	var parsed arguments
	values, err := decodeArgumentMap(raw)
	if err != nil {
		return parsed, err
	}
	if err := validateArgumentValues(values, fields); err != nil {
		return parsed, err
	}
	if err := requireArguments(values, required); err != nil {
		return parsed, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return parsed, &InvalidParams{Message: "Invalid arguments"}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return parsed, &InvalidParams{Message: "Invalid arguments"}
	}
	if _, ok := values["timeout"]; ok && (parsed.Timeout < 1 || parsed.Timeout > 3600) {
		return parsed, &InvalidParams{Message: "timeout must be between 1 and 3600"}
	}
	if parsed.Timeout == 0 {
		parsed.Timeout = 30
	}
	return parsed, nil
}

func decodeArgumentMap(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, &InvalidParams{Message: "Object arguments required"}
	}
	return values, nil
}

func validateArgumentValues(values map[string]json.RawMessage, fields []string) error {
	allowed := map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	for field, value := range values {
		if !allowed[field] || bytes.Equal(value, []byte("null")) {
			return &InvalidParams{Message: "Unexpected or null argument: " + field}
		}
		if field != "timeout" && !validTextArgument(field, value) {
			return &InvalidParams{Message: "Invalid argument: " + field}
		}
	}
	return nil
}

func validTextArgument(field string, value json.RawMessage) bool {
	var text string
	limit := 256
	if field == "command" || field == "content" {
		limit = 256 * 1024
	}
	return json.Unmarshal(value, &text) == nil && len(text) <= limit && (field == "content" || strings.TrimSpace(text) != "")
}

func requireArguments(values map[string]json.RawMessage, required []string) error {
	for _, field := range required {
		if _, ok := values[field]; !ok {
			return &InvalidParams{Message: "Missing argument: " + field}
		}
	}
	return nil
}

func dataResult(value any) (CallResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return CallResult{}, err
	}
	return CallResult{Content: []TextContent{{Type: "text", Text: string(encoded)}}, StructuredContent: json.RawMessage(encoded)}, nil
}

func protoResult(message proto.Message) (CallResult, error) {
	encoded, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(message)
	if err != nil {
		return CallResult{}, err
	}
	return dataResult(json.RawMessage(encoded))
}

func (service *Service) owner(ctx context.Context, principal Principal, kind, handle string) (Record, error) {
	record, err := service.store.Get(ctx, recordKey(kind, handle))
	if err != nil || record.Owner != principal.ID {
		return Record{}, errors.New("resource inaccessible")
	}
	return record, nil
}

func (service *Service) call(ctx context.Context, principal Principal, name string, parsed arguments) (CallResult, error) {
	if principal.ID == "" {
		return CallResult{}, errors.New("principal required")
	}
	if name == "sandbox_operation" {
		return service.recoverOperation(ctx, principal, parsed)
	}
	if name == "sandbox_poll" {
		return service.pollRun(ctx, principal, parsed.RunID)
	}
	if name != "sandbox_create" {
		if _, err := service.owner(ctx, principal, "session", parsed.SessionID); err != nil {
			return CallResult{}, err
		}
	}
	switch name {
	case "sandbox_get":
		response, err := service.backend.GetSession(ctx, &pb.GetSessionRequest{SessionId: parsed.SessionID})
		if err != nil {
			return CallResult{}, err
		}
		return protoResult(response)
	case "sandbox_read":
		response, err := service.backend.ReadFile(ctx, &pb.ReadFileRequest{SessionId: parsed.SessionID, Path: parsed.Path, Length: 256 * 1024, Encoding: "base64"})
		if err != nil {
			return CallResult{}, err
		}
		return protoResult(response)
	case "sandbox_list":
		response, err := service.backend.ListFiles(ctx, &pb.ListFilesRequest{SessionId: parsed.SessionID})
		if err != nil {
			return CallResult{}, err
		}
		return protoResult(response)
	default:
		return service.mutate(ctx, principal, name, parsed)
	}
}

func (service *Service) recoverOperation(ctx context.Context, principal Principal, parsed arguments) (CallResult, error) {
	record, err := service.store.Get(ctx, recordKey("operation", principal.ID, parsed.OperationKind, parsed.OperationID))
	if err != nil || record.Owner != principal.ID {
		return CallResult{}, errors.New("operation inaccessible")
	}
	return operationResult(record, parsed.OperationID)
}

func (service *Service) pollRun(ctx context.Context, principal Principal, runID string) (CallResult, error) {
	if _, err := service.owner(ctx, principal, "run", runID); err != nil {
		return CallResult{}, err
	}
	response, err := service.backend.PollRun(ctx, &pb.PollRunRequest{RunId: runID})
	if err != nil {
		return CallResult{}, err
	}
	return protoResult(response)
}

func (service *Service) executeMutation(ctx context.Context, principal Principal, name string, parsed arguments, record Record) (CallResult, error) {
	switch name {
	case "sandbox_create":
		return service.createSandbox(ctx, principal, record)
	case "sandbox_exec":
		return service.execSandbox(ctx, principal, parsed)
	case "sandbox_destroy":
		return service.destroySandbox(ctx, parsed)
	case "sandbox_write":
		return service.writeSandbox(ctx, parsed)
	default:
		return CallResult{}, fmt.Errorf("unsupported mutation")
	}
}

func (service *Service) createSandbox(ctx context.Context, principal Principal, record Record) (CallResult, error) {
	if _, err := service.store.Create(ctx, recordKey("session", record.SessionID), Record{Owner: principal.ID, State: "owned", SessionID: record.SessionID}); err != nil {
		return CallResult{}, err
	}
	response, err := service.backend.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: record.SessionID, RuntimeClass: "gvisor"})
	if err != nil {
		return CallResult{}, err
	}
	if response == nil || response.SessionId != record.SessionID {
		return CallResult{}, errors.New("backend changed session handle")
	}
	return dataResult(map[string]any{"session_id": record.SessionID})
}

func (service *Service) execSandbox(ctx context.Context, principal Principal, parsed arguments) (CallResult, error) {
	response, err := service.backend.Exec(ctx, &pb.ExecRequest{SessionId: parsed.SessionID, Command: parsed.Command, Timeout: parsed.Timeout, Workdir: "/workspace", Background: true})
	if err != nil {
		return CallResult{}, err
	}
	if response == nil || response.RunId == "" {
		return CallResult{}, errors.New("missing run handle")
	}
	if _, err := service.store.Create(ctx, recordKey("run", response.RunId), Record{Owner: principal.ID, State: "owned", SessionID: parsed.SessionID}); err != nil {
		return CallResult{}, err
	}
	return protoResult(response)
}

func (service *Service) destroySandbox(ctx context.Context, parsed arguments) (CallResult, error) {
	response, err := service.backend.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: parsed.SessionID})
	if err != nil {
		return CallResult{}, err
	}
	return protoResult(response)
}

func (service *Service) writeSandbox(ctx context.Context, parsed arguments) (CallResult, error) {
	response, err := service.backend.WriteFile(ctx, &pb.WriteFileRequest{SessionId: parsed.SessionID, Path: parsed.Path, Content: parsed.Content, Mode: "w"})
	if err != nil {
		return CallResult{}, err
	}
	return protoResult(response)
}

func operationResult(record Record, operationID string) (CallResult, error) {
	if record.State == "complete" {
		return dataResult(record.Result)
	}
	return dataResult(map[string]any{"status": "unknown", "operation_id": operationID, "session_id": record.SessionID, "retry_safe": false})
}

func (service *Service) mutate(ctx context.Context, principal Principal, name string, parsed arguments) (CallResult, error) {
	encoded, _ := json.Marshal(parsed)
	digest := sha256.Sum256(encoded)
	key := recordKey("operation", principal.ID, name, parsed.OperationID)
	record := Record{Owner: principal.ID, Digest: hex.EncodeToString(digest[:]), State: "unknown", SessionID: parsed.SessionID}
	if name == "sandbox_create" {
		identifier := make([]byte, 16)
		if _, err := rand.Read(identifier); err != nil {
			return CallResult{}, err
		}
		record.SessionID = "mcp-" + hex.EncodeToString(identifier)
	}
	created, err := service.store.Create(ctx, key, record)
	if errors.Is(err, ErrRecordExists) {
		previous, readErr := service.store.Get(ctx, key)
		if readErr != nil {
			return CallResult{}, readErr
		}
		if previous.Owner != principal.ID || previous.Digest != record.Digest {
			return CallResult{}, &InvalidParams{Message: "operation_id already used with different arguments"}
		}
		return operationResult(previous, parsed.OperationID)
	}
	if err != nil {
		return CallResult{}, err
	}
	record = created
	result, err := service.executeMutation(ctx, principal, name, parsed, record)
	if err != nil {
		return operationResult(record, parsed.OperationID)
	}
	record.Result, err = json.Marshal(result.StructuredContent)
	if err != nil {
		return operationResult(record, parsed.OperationID)
	}
	record.State = "complete"
	if err = service.store.Update(ctx, key, record); err != nil {
		record.State = "unknown"
		return operationResult(record, parsed.OperationID)
	}
	return result, nil
}
