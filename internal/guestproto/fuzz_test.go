package guestproto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func FuzzGuestProtocol(f *testing.F) {
	validRequest := `{"protocol":"dsx.guest/v1","request_id":"01890f5c-7b00-7000-8000-000000000001","operation":"ping","deadline_ms":5000,"params":{}}`
	validResponse := `{"protocol":"dsx.guest/v1","request_id":"01890f5c-7b00-7000-8000-000000000001","ok":true,"result":{},"error":null,"server":{"instance_id":"01890f5c-7b00-7000-8000-000000000003","version":"dev"}}`
	for _, seed := range [][]byte{
		[]byte(validRequest),
		[]byte(validResponse),
		[]byte(validRequest[:len(validRequest)-1]),
		[]byte(strings.Replace(validRequest, `"operation":"ping"`, `"operation":"ping","operation":"status"`, 1)),
		[]byte(strings.Replace(validRequest, `"params":{}`, `"params":{"message":"\u001b[2J"}`, 1)),
		{0xff, 0xfe, '{', '}'},
		[]byte(strings.Repeat(" ", MaxFrameSize+1)),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, frame []byte) {
		if len(frame) > MaxFrameSize+1 {
			t.Skip()
		}
		before := bytes.Clone(frame)
		request, requestErr := DecodeRequest(frame)
		response, responseErr := DecodeResponse(frame)
		if !bytes.Equal(frame, before) {
			t.Fatal("protocol decoders mutated their input frame")
		}
		if requestErr == nil {
			encoded, err := EncodeRequest(request)
			if err != nil {
				t.Fatalf("accepted request could not be re-encoded: %v", err)
			}
			if _, err := DecodeRequest(encoded); err != nil {
				t.Fatalf("accepted request did not round trip: %v", err)
			}
		}
		if responseErr == nil {
			encoded, err := EncodeResponse(response)
			if err != nil {
				t.Fatalf("accepted response could not be re-encoded: %v", err)
			}
			if _, err := DecodeResponse(encoded); err != nil {
				t.Fatalf("accepted response did not round trip: %v", err)
			}
		}
	})
}

func FuzzGuestFrame(f *testing.F) {
	valid := make([]byte, 4+2)
	binary.BigEndian.PutUint32(valid[:4], 2)
	copy(valid[4:], `{}`)
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, MaxFrameSize+1)
	for _, seed := range [][]byte{
		valid,
		valid[:len(valid)-1],
		{0, 0, 0, 0},
		oversize,
		{0xff, 0xff, 0xff, 0xff},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > MaxFrameSize+frameHeaderBytes+1 {
			t.Skip()
		}
		before := bytes.Clone(stream)
		frame, err := ReadFrame(bytes.NewReader(stream))
		if !bytes.Equal(stream, before) {
			t.Fatal("ReadFrame mutated its input stream")
		}
		if err == nil && (len(frame) == 0 || len(frame) > MaxFrameSize) {
			t.Fatalf("ReadFrame accepted invalid payload size %d", len(frame))
		}
	})
}
