package guestproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

const (
	testRequestID = "01890f5c-7b00-7000-8000-000000000001"
	testKey       = "01890f5c-7b00-7000-8000-000000000002"
	testInstance  = "01890f5c-7b00-7000-8000-000000000003"
)

func TestRequestGoldenRoundTrip(t *testing.T) {
	generation := uint64(3)
	request := Request{
		Protocol: ProtocolV1, RequestID: testRequestID, Operation: OperationSignal,
		Target: "web", IfGeneration: &generation, IdempotencyKey: testKey,
		DeadlineMS: 5000, Params: json.RawMessage(`{"signal":"TERM"}`),
	}
	encoded, err := EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol":"dsx.guest/v1","request_id":"01890f5c-7b00-7000-8000-000000000001","operation":"signal","target":"web","if_generation":3,"idempotency_key":"01890f5c-7b00-7000-8000-000000000002","deadline_ms":5000,"params":{"signal":"TERM"}}`
	if string(encoded) != want {
		t.Fatalf("EncodeRequest() = %s\nwant %s", encoded, want)
	}
	decoded, err := DecodeRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("DecodeRequest() = %#v, want %#v", decoded, request)
	}
}

func TestResponseGoldenRoundTrip(t *testing.T) {
	generation := uint64(4)
	response := Response{
		Protocol: ProtocolV1, RequestID: testRequestID, OK: false,
		Result: json.RawMessage(`null`),
		Error:  &Error{Code: CodeGenerationConflict, Message: "generation changed", Generation: &generation},
		Server: Server{InstanceID: testInstance, Version: "dev"},
	}
	encoded, err := EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol":"dsx.guest/v1","request_id":"01890f5c-7b00-7000-8000-000000000001","ok":false,"result":null,"error":{"code":"generation_conflict","message":"generation changed","generation":4},"server":{"instance_id":"01890f5c-7b00-7000-8000-000000000003","version":"dev"}}`
	if string(encoded) != want {
		t.Fatalf("EncodeResponse() = %s\nwant %s", encoded, want)
	}
	decoded, err := DecodeResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, response) {
		t.Fatalf("DecodeResponse() = %#v, want %#v", decoded, response)
	}
}

func TestFrameRoundTripAndBounds(t *testing.T) {
	payload := []byte(`{"ok":true}`)
	var stream bytes.Buffer
	if err := WriteFrame(&stream, payload); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(stream.Bytes()[:4]); got != uint32(len(payload)) {
		t.Fatalf("frame length = %d", got)
	}
	decoded, err := ReadFrame(&stream)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("ReadFrame() = %q, %v", decoded, err)
	}
	for _, size := range []uint32{0, MaxFrameSize + 1} {
		var invalid bytes.Buffer
		if err := binary.Write(&invalid, binary.BigEndian, size); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFrame(&invalid); ErrorCodeOf(err) != CodeInvalidRequest {
			t.Fatalf("ReadFrame(size=%d) error = %v", size, err)
		}
	}
	short := bytes.NewBuffer([]byte{0, 0, 0, 4, 1, 2})
	if _, err := ReadFrame(short); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short frame error = %v", err)
	}
}

func TestDecodeRequestRejectsUnknownDuplicateMalformedAndUnsupported(t *testing.T) {
	valid := `{"protocol":"dsx.guest/v1","request_id":"` + testRequestID + `","operation":"ping","deadline_ms":5000,"params":{}}`
	cases := map[string]string{
		"unknown":     strings.TrimSuffix(valid, `}`) + `,"extra":true}`,
		"duplicate":   strings.Replace(valid, `"operation":"ping"`, `"operation":"ping","operation":"status"`, 1),
		"unsupported": strings.Replace(valid, ProtocolV1, "dsx.guest/v2", 1),
		"operation":   strings.Replace(valid, `"operation":"ping"`, `"operation":"exec"`, 1),
		"trailing":    valid + `{}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(input))
			if err == nil {
				t.Fatalf("DecodeRequest(%s) succeeded", input)
			}
			if name == "unsupported" && ErrorCodeOf(err) != CodeUnsupportedProtocol {
				t.Fatalf("unsupported code = %q, error %v", ErrorCodeOf(err), err)
			}
		})
	}
}

func TestRequestMutationGenerationAndIdempotencyRules(t *testing.T) {
	base := Request{Protocol: ProtocolV1, RequestID: testRequestID, DeadlineMS: 5000, Params: json.RawMessage(`{}`)}
	cases := []Request{
		withOperation(base, OperationStart, ""),
		withOperation(base, OperationSignal, "web"),
		withOperation(base, OperationResize, "web"),
		withOperation(base, OperationShutdown, ""),
	}
	for _, request := range cases {
		if err := request.Validate(); ErrorCodeOf(err) != CodeInvalidRequest {
			t.Fatalf("%s without idempotency error = %v", request.Operation, err)
		}
		request.IdempotencyKey = testKey
		if request.Operation == OperationStart || request.Operation == OperationSignal || request.Operation == OperationResize {
			generation := uint64(1)
			request.IfGeneration = &generation
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("valid %s rejected: %v", request.Operation, err)
		}
	}
	nonMutation := withOperation(base, OperationPing, "")
	nonMutation.IdempotencyKey = testKey
	if err := nonMutation.Validate(); ErrorCodeOf(err) != CodeInvalidRequest {
		t.Fatalf("non-mutation idempotency error = %v", err)
	}
}

func TestDecodeParamsRejectsUnknownAndDuplicateFields(t *testing.T) {
	type signalParams struct {
		Signal string `json:"signal"`
	}
	var params signalParams
	if err := DecodeParams(json.RawMessage(`{"signal":"TERM"}`), &params); err != nil || params.Signal != "TERM" {
		t.Fatalf("DecodeParams() = %#v, %v", params, err)
	}
	for _, input := range []string{`{"signal":"TERM","extra":true}`, `{"signal":"TERM","signal":"KILL"}`} {
		if err := DecodeParams(json.RawMessage(input), &params); ErrorCodeOf(err) != CodeInvalidRequest {
			t.Fatalf("DecodeParams(%s) error = %v", input, err)
		}
	}
}

func TestResponseRejectsContradictorySuccessAndInvalidErrors(t *testing.T) {
	base := Response{Protocol: ProtocolV1, RequestID: testRequestID, Result: json.RawMessage(`null`), Server: Server{InstanceID: testInstance, Version: "dev"}}
	base.OK = true
	base.Error = &Error{Code: CodeInternal, Message: "not allowed"}
	if err := base.Validate(); ErrorCodeOf(err) != CodeInvalidRequest {
		t.Fatalf("successful error response accepted: %v", err)
	}
	base.OK = false
	base.Error = &Error{Code: "unknown", Message: "bad"}
	if err := base.Validate(); ErrorCodeOf(err) != CodeInvalidRequest {
		t.Fatalf("unknown error code accepted: %v", err)
	}
}

func withOperation(request Request, operation Operation, target string) Request {
	request.Operation = operation
	request.Target = target
	return request
}
