package guestproto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/srimajji/dsx/internal/model"
)

const (
	ProtocolV1           = "dsx.guest/v1"
	MaxFrameSize         = 1 << 20
	MaxDeadlineMS        = 60_000
	maxJSONDepth         = 64
	maxJSONMembers       = 4096
	frameHeaderBytes     = 4
	DefaultHelperPath    = "/usr/local/bin/dsx-guest"
	DefaultSocketPath    = "/run/dsx/control.sock"
	DefaultSocketDir     = "/run/dsx"
	DefaultSocketMode    = 0o660
	DefaultSocketDirMode = 0o750
)

type Operation string

const (
	OperationPing     Operation = "ping"
	OperationStatus   Operation = "status"
	OperationStart    Operation = "start"
	OperationSignal   Operation = "signal"
	OperationResize   Operation = "resize"
	OperationWait     Operation = "wait"
	OperationShutdown Operation = "shutdown"
)

type ErrorCode string

const (
	CodeInvalidRequest      ErrorCode = "invalid_request"
	CodeUnsupportedProtocol ErrorCode = "unsupported_protocol"
	CodeNotFound            ErrorCode = "not_found"
	CodeWrongState          ErrorCode = "wrong_state"
	CodeGenerationConflict  ErrorCode = "generation_conflict"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeStartFailed         ErrorCode = "start_failed"
	CodeSignalFailed        ErrorCode = "signal_failed"
	CodeResizeFailed        ErrorCode = "resize_failed"
	CodeDeadlineExceeded    ErrorCode = "deadline_exceeded"
	CodePermissionDenied    ErrorCode = "permission_denied"
	CodeShuttingDown        ErrorCode = "shutting_down"
	CodeInternal            ErrorCode = "internal"
)

type Request struct {
	Protocol       string          `json:"protocol"`
	RequestID      string          `json:"request_id"`
	Operation      Operation       `json:"operation"`
	Target         string          `json:"target,omitempty"`
	IfGeneration   *uint64         `json:"if_generation,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	DeadlineMS     uint32          `json:"deadline_ms"`
	Params         json.RawMessage `json:"params"`
}

type Response struct {
	Protocol  string          `json:"protocol"`
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result"`
	Error     *Error          `json:"error"`
	Server    Server          `json:"server"`
}

type Error struct {
	Code       ErrorCode       `json:"code"`
	Message    string          `json:"message"`
	Generation *uint64         `json:"generation,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

type Server struct {
	InstanceID string `json:"instance_id"`
	Version    string `json:"version"`
}

type ProtocolError struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (err *ProtocolError) Error() string {
	if err == nil {
		return ""
	}
	if err.Cause == nil {
		return err.Message
	}
	return err.Message + ": " + err.Cause.Error()
}

func (err *ProtocolError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func ReadFrame(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, protocolError(CodeInvalidRequest, "frame reader is nil", nil)
	}
	var header [frameHeaderBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, protocolError(CodeInvalidRequest, "read frame header", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrameSize {
		return nil, protocolError(CodeInvalidRequest, fmt.Sprintf("frame size %d is outside 1..%d", size, MaxFrameSize), nil)
	}
	frame := make([]byte, int(size))
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, protocolError(CodeInvalidRequest, "read frame payload", err)
	}
	return frame, nil
}

func WriteFrame(writer io.Writer, frame []byte) error {
	if writer == nil {
		return protocolError(CodeInvalidRequest, "frame writer is nil", nil)
	}
	if len(frame) == 0 || len(frame) > MaxFrameSize {
		return protocolError(CodeInvalidRequest, fmt.Sprintf("frame size %d is outside 1..%d", len(frame), MaxFrameSize), nil)
	}
	var header [frameHeaderBytes]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(frame)))
	if err := writeAll(writer, header[:]); err != nil {
		return protocolError(CodeInternal, "write frame header", err)
	}
	if err := writeAll(writer, frame); err != nil {
		return protocolError(CodeInternal, "write frame payload", err)
	}
	return nil
}

func ReadRequest(reader io.Reader) (Request, error) {
	frame, err := ReadFrame(reader)
	if err != nil {
		return Request{}, err
	}
	return DecodeRequest(frame)
}

func WriteRequest(writer io.Writer, request Request) error {
	frame, err := EncodeRequest(request)
	if err != nil {
		return err
	}
	return WriteFrame(writer, frame)
}

func ReadResponse(reader io.Reader) (Response, error) {
	frame, err := ReadFrame(reader)
	if err != nil {
		return Response{}, err
	}
	return DecodeResponse(frame)
}

func WriteResponse(writer io.Writer, response Response) error {
	frame, err := EncodeResponse(response)
	if err != nil {
		return err
	}
	return WriteFrame(writer, frame)
}

func DecodeRequest(frame []byte) (Request, error) {
	var request Request
	if err := decodeStrict(frame, &request); err != nil {
		return request, protocolError(CodeInvalidRequest, "decode request", err)
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func EncodeRequest(request Request) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return encodeBounded(request)
}

func DecodeResponse(frame []byte) (Response, error) {
	var response Response
	if err := decodeStrict(frame, &response); err != nil {
		return response, protocolError(CodeInvalidRequest, "decode response", err)
	}
	if err := response.Validate(); err != nil {
		return response, err
	}
	return response, nil
}

func EncodeResponse(response Response) ([]byte, error) {
	if err := response.Validate(); err != nil {
		return nil, err
	}
	return encodeBounded(response)
}

func (request Request) Validate() error {
	if request.Protocol != ProtocolV1 {
		return protocolError(CodeUnsupportedProtocol, fmt.Sprintf("protocol %q is unsupported", request.Protocol), nil)
	}
	if !ValidUUID(request.RequestID) {
		return protocolError(CodeInvalidRequest, "request_id must be a canonical UUID", nil)
	}
	if request.DeadlineMS == 0 || request.DeadlineMS > MaxDeadlineMS {
		return protocolError(CodeInvalidRequest, fmt.Sprintf("deadline_ms must be within 1..%d", MaxDeadlineMS), nil)
	}
	if len(request.Params) == 0 || !jsonObject(request.Params) {
		return protocolError(CodeInvalidRequest, "params must be a JSON object", nil)
	}
	if err := validateJSON(request.Params); err != nil {
		return protocolError(CodeInvalidRequest, "params are invalid", err)
	}

	mutation := false
	requiresTarget := false
	switch request.Operation {
	case OperationPing, OperationStatus:
	case OperationStart:
		mutation = true
		if request.Target != "" || request.IfGeneration == nil {
			return protocolError(CodeInvalidRequest, "start requires if_generation and does not accept a target", nil)
		}
	case OperationSignal, OperationResize:
		mutation = true
		requiresTarget = true
		if request.IfGeneration == nil {
			return protocolError(CodeInvalidRequest, "if_generation is required for signal and resize", nil)
		}
	case OperationWait:
		requiresTarget = true
	case OperationShutdown:
		mutation = true
		if request.Target != "" {
			return protocolError(CodeInvalidRequest, "shutdown does not accept a target", nil)
		}
	default:
		return protocolError(CodeInvalidRequest, fmt.Sprintf("operation %q is unknown", request.Operation), nil)
	}
	if requiresTarget {
		parsed, err := model.ParseWorkspaceName(request.Target)
		if err != nil || string(parsed) != request.Target {
			return protocolError(CodeInvalidRequest, "target is not a valid configured process ID", err)
		}
	} else if request.Target != "" {
		parsed, err := model.ParseWorkspaceName(request.Target)
		if err != nil || string(parsed) != request.Target {
			return protocolError(CodeInvalidRequest, "target is invalid", err)
		}
	}
	if mutation {
		if !ValidUUID(request.IdempotencyKey) {
			return protocolError(CodeInvalidRequest, "mutation requires a canonical UUID idempotency_key", nil)
		}
	} else if request.IdempotencyKey != "" {
		return protocolError(CodeInvalidRequest, "non-mutation request must not include idempotency_key", nil)
	}
	if request.IfGeneration != nil && request.Operation != OperationStart && request.Operation != OperationSignal && request.Operation != OperationResize && request.Operation != OperationWait {
		return protocolError(CodeInvalidRequest, "if_generation is not valid for this operation", nil)
	}
	return nil
}

func (response Response) Validate() error {
	if response.Protocol != ProtocolV1 {
		return protocolError(CodeUnsupportedProtocol, fmt.Sprintf("protocol %q is unsupported", response.Protocol), nil)
	}
	if !ValidUUID(response.RequestID) {
		return protocolError(CodeInvalidRequest, "request_id must be a canonical UUID", nil)
	}
	if !ValidUUID(response.Server.InstanceID) || strings.TrimSpace(response.Server.Version) == "" || response.Server.Version != strings.TrimSpace(response.Server.Version) {
		return protocolError(CodeInvalidRequest, "server identity is invalid", nil)
	}
	if response.OK {
		if response.Error != nil {
			return protocolError(CodeInvalidRequest, "successful response contains an error", nil)
		}
	} else {
		if response.Error == nil || !response.Error.Code.Valid() || strings.TrimSpace(response.Error.Message) == "" {
			return protocolError(CodeInvalidRequest, "failed response has no valid error", nil)
		}
	}
	if len(response.Result) != 0 && string(response.Result) != "null" {
		if err := validateJSON(response.Result); err != nil {
			return protocolError(CodeInvalidRequest, "response result is invalid", err)
		}
	}
	if response.Error != nil && len(response.Error.Details) != 0 {
		if err := validateJSON(response.Error.Details); err != nil {
			return protocolError(CodeInvalidRequest, "response error details are invalid", err)
		}
	}
	return nil
}

func (code ErrorCode) Valid() bool {
	switch code {
	case CodeInvalidRequest, CodeUnsupportedProtocol, CodeNotFound, CodeWrongState,
		CodeGenerationConflict, CodeIdempotencyConflict, CodeStartFailed, CodeSignalFailed,
		CodeResizeFailed, CodeDeadlineExceeded, CodePermissionDenied, CodeShuttingDown, CodeInternal:
		return true
	default:
		return false
	}
}

func ErrorCodeOf(err error) ErrorCode {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return protocolErr.Code
	}
	return CodeInternal
}

func ValidUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.NewReplacer("-", "").Replace(value)
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'F' {
			return false
		}
	}
	return true
}

func DecodeParams(raw json.RawMessage, destination any) error {
	if destination == nil {
		return protocolError(CodeInvalidRequest, "params destination is nil", nil)
	}
	if !jsonObject(raw) {
		return protocolError(CodeInvalidRequest, "params must be a JSON object", nil)
	}
	if err := decodeStrict(raw, destination); err != nil {
		return protocolError(CodeInvalidRequest, "decode params", err)
	}
	return nil
}

func protocolError(code ErrorCode, message string, cause error) error {
	return &ProtocolError{Code: code, Message: message, Cause: cause}
}

func encodeBounded(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, protocolError(CodeInternal, "encode JSON", err)
	}
	if len(encoded) == 0 || len(encoded) > MaxFrameSize {
		return nil, protocolError(CodeInvalidRequest, fmt.Sprintf("encoded frame size %d is outside 1..%d", len(encoded), MaxFrameSize), nil)
	}
	return encoded, nil
}

func decodeStrict(data []byte, destination any) error {
	if len(data) == 0 || len(data) > MaxFrameSize {
		return fmt.Errorf("JSON size %d is outside 1..%d", len(data), MaxFrameSize)
	}
	if err := validateJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := consumeEOF(decoder); err != nil {
		return err
	}
	return nil
}

func validateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	members := 0
	if err := scanJSONValue(decoder, token, 0, &members); err != nil {
		return err
	}
	return consumeEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder, token json.Token, depth int, members *int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON depth exceeds %d", maxJSONDepth)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			*members++
			if *members > maxJSONMembers {
				return fmt.Errorf("JSON members exceed %d", maxJSONMembers)
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken, depth+1, members); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object close: %v", err)
		}
	case '[':
		for decoder.More() {
			*members++
			if *members > maxJSONMembers {
				return fmt.Errorf("JSON members exceed %d", maxJSONMembers)
			}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken, depth+1, members); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array close: %v", err)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func consumeEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func jsonObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
