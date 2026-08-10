package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
)

var (
	ErrControlInput     = errors.New("invalid control input")
	ErrControlTransport = errors.New("control transport failed")
)

func Control(ctx context.Context, socketPath string, input io.Reader, output io.Writer) (bool, error) {
	if input == nil || output == nil {
		return false, fmt.Errorf("%w: stdin and stdout are required", ErrControlInput)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	raw, err := io.ReadAll(io.LimitReader(input, guestproto.MaxFrameSize+1))
	if err != nil {
		return false, fmt.Errorf("%w: read request: %v", ErrControlInput, err)
	}
	if len(raw) > guestproto.MaxFrameSize {
		return false, fmt.Errorf("%w: request exceeds %d bytes", ErrControlInput, guestproto.MaxFrameSize)
	}
	raw = bytes.TrimSpace(raw)
	request, err := guestproto.DecodeRequest(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrControlInput, err)
	}
	frame, err := guestproto.EncodeRequest(request)
	if err != nil {
		return false, fmt.Errorf("%w: encode request: %v", ErrControlInput, err)
	}
	deadline := time.Now().Add(time.Duration(request.DeadlineMS) * time.Millisecond)
	if parentDeadline, found := ctx.Deadline(); found && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	dialer := net.Dialer{}
	dialContext, cancelDial := context.WithDeadline(ctx, deadline)
	defer cancelDial()
	connection, err := dialer.DialContext(dialContext, "unix", socketPath)
	if err != nil {
		return false, fmt.Errorf("%w: connect: %v", ErrControlTransport, err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(deadline); err != nil {
		return false, fmt.Errorf("%w: set deadline: %v", ErrControlTransport, err)
	}
	if err := guestproto.WriteFrame(connection, frame); err != nil {
		return false, fmt.Errorf("%w: write request: %v", ErrControlTransport, err)
	}
	responseFrame, err := guestproto.ReadFrame(connection)
	if err != nil {
		return false, fmt.Errorf("%w: read response: %v", ErrControlTransport, err)
	}
	response, err := guestproto.DecodeResponse(responseFrame)
	if err != nil {
		return false, fmt.Errorf("%w: decode response: %v", ErrControlTransport, err)
	}
	if response.RequestID != request.RequestID {
		return false, fmt.Errorf("%w: response request ID mismatch", ErrControlTransport)
	}
	compact, err := guestproto.EncodeResponse(response)
	if err != nil {
		return false, fmt.Errorf("%w: encode response: %v", ErrControlTransport, err)
	}
	compact = append(compact, '\n')
	if err := writeAll(output, compact); err != nil {
		return false, fmt.Errorf("%w: write response: %v", ErrControlTransport, err)
	}
	return response.OK, nil
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
