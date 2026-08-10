package guest

import (
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	outputQueueDepth = 64
	maxOutputChunk   = 32 << 10
)

type processLog struct {
	mu            sync.Mutex
	id            string
	prefix        []byte
	limit         int
	bytes         []byte
	dropped       uint64
	atLineStart   bool
	output        *asyncOutput
	outputDropped uint64
}

func newProcessLog(id string, limit int, output *asyncOutput) *processLog {
	return &processLog{id: id, prefix: []byte("[" + id + "] "), limit: limit, atLineStart: true, output: output}
}

func (log *processLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	formatted := log.prefixBytes(data)
	log.appendBounded(formatted)
	if len(formatted) != 0 && log.output != nil {
		if log.outputDropped != 0 {
			marker := []byte(fmt.Sprintf("[%s] [output dropped %d bytes]\n", log.id, log.outputDropped))
			if accepted := log.output.Enqueue(marker); accepted == len(marker) {
				log.outputDropped = 0
			}
		}
		accepted := log.output.Enqueue(formatted)
		if accepted < len(formatted) {
			dropped := uint64(len(formatted) - accepted)
			log.outputDropped += dropped
			log.dropped += dropped
		}
	}
	return len(data), nil
}

func (log *processLog) Flush() {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.outputDropped == 0 || log.output == nil {
		return
	}
	marker := []byte(fmt.Sprintf("[%s] [output dropped %d bytes]\n", log.id, log.outputDropped))
	log.appendBounded(marker)
	if accepted := log.output.Enqueue(marker); accepted == len(marker) {
		log.outputDropped = 0
	}
}

func (log *processLog) Snapshot() (string, uint64) {
	log.mu.Lock()
	defer log.mu.Unlock()
	return string(log.bytes), log.dropped
}

func (log *processLog) SnapshotLimit(limit int) (string, uint64) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if limit < 0 {
		limit = 0
	}
	start := len(log.bytes) - limit
	if start < 0 {
		start = 0
	}
	return string(log.bytes[start:]), log.dropped + uint64(start)
}

func (log *processLog) prefixBytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	prefixes := 0
	lineStart := log.atLineStart
	for _, character := range data {
		if lineStart {
			prefixes++
			lineStart = false
		}
		if character == '\n' {
			lineStart = true
		}
	}
	formatted := make([]byte, 0, len(data)+prefixes*len(log.prefix))
	for _, character := range data {
		if log.atLineStart {
			formatted = append(formatted, log.prefix...)
			log.atLineStart = false
		}
		formatted = append(formatted, character)
		if character == '\n' {
			log.atLineStart = true
		}
	}
	return formatted
}

func (log *processLog) appendBounded(data []byte) {
	if len(data) == 0 {
		return
	}
	if len(log.bytes)+len(data) <= log.limit {
		log.bytes = append(log.bytes, data...)
		return
	}
	combined := make([]byte, 0, len(log.bytes)+len(data))
	combined = append(combined, log.bytes...)
	combined = append(combined, data...)
	overflow := len(combined) - log.limit
	if overflow < 0 {
		overflow = 0
	}
	candidate := combined[overflow:]
	for index, character := range candidate {
		if character == '\n' && index+1 < len(candidate) {
			overflow += index + 1
			candidate = combined[overflow:]
			break
		}
	}
	if len(candidate) > log.limit {
		candidate = candidate[len(candidate)-log.limit:]
	}
	if len(candidate) != 0 && candidate[0] != '[' && log.limit > len(log.prefix) {
		keep := log.limit - len(log.prefix)
		if keep > len(combined) {
			keep = len(combined)
		}
		rebuilt := make([]byte, 0, len(log.prefix)+keep)
		rebuilt = append(rebuilt, log.prefix...)
		rebuilt = append(rebuilt, combined[len(combined)-keep:]...)
		candidate = rebuilt
	}
	removed := len(combined) - len(candidate)
	if removed > 0 {
		log.dropped += uint64(removed)
	}
	log.bytes = append(log.bytes[:0], candidate...)
}

type asyncOutput struct {
	mu     sync.Mutex
	writer io.Writer
	queue  chan []byte
	done   chan struct{}
	closed bool
}

func newAsyncOutput(writer io.Writer) *asyncOutput {
	output := &asyncOutput{writer: writer, queue: make(chan []byte, outputQueueDepth), done: make(chan struct{})}
	if writer != nil {
		go output.run()
	} else {
		close(output.done)
	}
	return output
}

func (output *asyncOutput) Enqueue(data []byte) int {
	if output == nil || output.writer == nil || len(data) == 0 {
		return len(data)
	}
	accepted := len(data)
	if accepted > maxOutputChunk {
		accepted = maxOutputChunk
	}
	copyData := append([]byte(nil), data[:accepted]...)
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.closed {
		return 0
	}
	select {
	case output.queue <- copyData:
		return accepted
	default:
		return 0
	}
}

func (output *asyncOutput) Close() {
	if output == nil || output.writer == nil {
		return
	}
	output.mu.Lock()
	if !output.closed {
		output.closed = true
		close(output.queue)
	}
	output.mu.Unlock()
	select {
	case <-output.done:
	case <-time.After(100 * time.Millisecond):
	}
}

func (output *asyncOutput) run() {
	defer close(output.done)
	for data := range output.queue {
		_, _ = output.writer.Write(data)
	}
}
