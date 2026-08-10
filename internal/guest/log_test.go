package guest

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
)

func TestProcessLogBoundsSmallLimit(t *testing.T) {
	log := newProcessLog("x", 1, newAsyncOutput(nil))
	if _, err := log.Write([]byte("many bytes\n")); err != nil {
		t.Fatal(err)
	}
	contents, dropped := log.Snapshot()
	if len(contents) > 1 || dropped == 0 {
		t.Fatalf("small ring = %q (%d bytes), dropped %d", contents, len(contents), dropped)
	}
}

func TestProcessLogDoesNotBlockOnSlowOutputAndCountsDrops(t *testing.T) {
	writer := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	output := newAsyncOutput(writer)
	log := newProcessLog("busy", 512, output)
	if _, err := log.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("output writer was not entered")
	}
	started := time.Now()
	for index := 0; index < outputQueueDepth+100; index++ {
		if _, err := log.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("logging blocked behind output for %v", elapsed)
	}
	log.Flush()
	contents, dropped := log.Snapshot()
	if dropped == 0 || !strings.Contains(contents, "[busy] [output dropped ") {
		t.Fatalf("backpressure log = %q, dropped %d", contents, dropped)
	}
	output.Close()
	close(writer.release)
}

func TestProcessLogCountsOnlyOversizedOutputTailAsDropped(t *testing.T) {
	var writer bytes.Buffer
	output := newAsyncOutput(&writer)
	log := newProcessLog("wide", maxOutputChunk*2, output)
	data := []byte(strings.Repeat("x", maxOutputChunk+100))
	if _, err := log.Write(data); err != nil {
		t.Fatal(err)
	}
	output.Close()
	formattedBytes := len(data) + len("[wide] ")
	if writer.Len() != maxOutputChunk {
		t.Fatalf("accepted output bytes = %d, want %d", writer.Len(), maxOutputChunk)
	}
	_, dropped := log.Snapshot()
	if want := uint64(formattedBytes - maxOutputChunk); dropped != want {
		t.Fatalf("dropped bytes = %d, want %d", dropped, want)
	}
}

func TestBoundedStatusResponseTruncatesAggregateLogs(t *testing.T) {
	status := guestproto.StatusResult{Processes: []guestproto.ProcessStatus{
		{ID: "one", Log: strings.Repeat("a", 700_000)},
		{ID: "two", Log: strings.Repeat("b", 700_000)},
	}}
	encoded, err := boundedStatusJSON(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > guestproto.MaxFrameSize-8192 {
		t.Fatalf("status result is %d bytes", len(encoded))
	}
	var bounded guestproto.StatusResult
	if err := json.Unmarshal(encoded, &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.Processes[0].LogDropped+bounded.Processes[1].LogDropped == 0 {
		t.Fatal("aggregate status truncation was not counted")
	}
}

type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockingWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.release
	return len(data), nil
}
