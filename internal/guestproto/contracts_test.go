package guestproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStartParamsValidateGraphAndCommands(t *testing.T) {
	valid := StartParams{
		Setup: []CommandSpec{{Argv: []string{"/bin/true"}, Cwd: "/workspace"}},
		Processes: []ProcessSpec{
			{ID: "db", Command: CommandSpec{Argv: []string{"redis-server"}, Cwd: "/workspace"}, Required: true, Health: &HealthSpec{Kind: "tcp", Target: "127.0.0.1:6379", IntervalMS: 10, TimeoutMS: 10, Retries: 3}},
			{ID: "web", Command: CommandSpec{Argv: []string{"node", "server.js"}, Cwd: "/workspace", Env: []string{"PORT=3000"}}, DependsOn: []string{"db"}, Required: true, Health: &HealthSpec{Kind: "http", Target: "http://127.0.0.1:3000/health", IntervalMS: 10, TimeoutMS: 10, Retries: 3}},
		},
		LogLimitBytes: 4096,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cycle := valid
	cycle.Processes = append([]ProcessSpec(nil), valid.Processes...)
	cycle.Processes[0].DependsOn = []string{"web"}
	if err := cycle.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	badEnv := valid
	badEnv.Processes = append([]ProcessSpec(nil), valid.Processes...)
	badEnv.Processes[1].Command.Env = []string{"TOKEN=one", "TOKEN=two"}
	if err := badEnv.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate environment") {
		t.Fatalf("environment error = %v", err)
	}
}

func TestStartRequestRequiresGenerationAndStrictParams(t *testing.T) {
	generation := uint64(0)
	request := Request{Protocol: ProtocolV1, RequestID: testRequestID, Operation: OperationStart, IfGeneration: &generation, IdempotencyKey: testKey, DeadlineMS: 1000, Params: json.RawMessage(`{"processes":[],"log_limit_bytes":1}`)}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.IfGeneration = nil
	if err := request.Validate(); ErrorCodeOf(err) != CodeInvalidRequest {
		t.Fatalf("missing generation error = %v", err)
	}
	var params StartParams
	if err := DecodeParams(json.RawMessage(`{"processes":[],"log_limit_bytes":1,"unknown":true}`), &params); ErrorCodeOf(err) != CodeInvalidRequest {
		t.Fatalf("unknown params error = %v", err)
	}
}
