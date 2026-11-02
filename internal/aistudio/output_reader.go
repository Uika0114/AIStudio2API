package aistudio

import (
	"encoding/json"
	"io"
	"time"
)

const maxOutputReadSpans = 2048

type outputByteSpan struct {
	start, end int64
	batch      outputReadBatch
}

type outputBatchReader struct {
	source io.Reader
	clock  func() time.Time
	batch  outputReadBatch
}

func (reader *outputBatchReader) Read(destination []byte) (int, error) {
	count, err := reader.source.Read(destination)
	if count > 0 {
		reader.batch = outputReadBatch{id: reader.batch.id + 1, at: reader.clock()}
	}
	return count, err
}

// outputDecoderTiming maps normalized JSON offsets back to real source reads.
// It stores byte ranges and monotonic timestamps, never the bytes themselves.
// Only the parser goroutine accesses the map; window snapshots have their own lock.
type outputDecoderTiming struct {
	window      *outputWindow
	reader      *outputBatchReader
	spans       []outputByteSpan
	offset      int64
	frame       outputReadBatch
	paired      bool
	complete    bool
	mappingDone bool
	clock       func() time.Time
}

func (timing *outputDecoderTiming) source(source io.Reader) io.Reader {
	if timing == nil {
		return source
	}
	if !knownOutputReadSource(source) {
		timing.window.unavailable("reader_source_unqualified")
		return source
	}
	clock := timing.clock
	if clock == nil {
		clock = time.Now
	}
	timing.reader = &outputBatchReader{source: source, clock: clock}
	return timing.reader
}

func (timing *outputDecoderTiming) currentBatch() outputReadBatch {
	if timing == nil || timing.reader == nil {
		return outputReadBatch{}
	}
	return timing.reader.batch
}

func (timing *outputDecoderTiming) normalized(count int, batch outputReadBatch) {
	if timing == nil {
		return
	}
	start := timing.offset
	timing.offset += int64(count)
	if timing.reader == nil || timing.mappingDone || count == 0 {
		return
	}
	if n := len(timing.spans); n != 0 && timing.spans[n-1].batch.id == batch.id && timing.spans[n-1].end == start {
		timing.spans[n-1].end = timing.offset
		return
	}
	if len(timing.spans) >= maxOutputReadSpans {
		// A huge cross-read frame is already ineligible as an endpoint.
		// Forget only old ranges, not the rest of this attempt's future.
		keep := len(timing.spans) / 2
		copy(timing.spans, timing.spans[keep:])
		timing.spans = timing.spans[:len(timing.spans)-keep]
	}
	timing.spans = append(timing.spans, outputByteSpan{start: start, end: timing.offset, batch: batch})
}

func (timing *outputDecoderTiming) frameRange(end int64, length int) {
	if timing == nil {
		return
	}
	timing.frame, timing.paired = outputReadBatch{}, false
	start := end - int64(length)
	if timing.reader == nil || start < 0 || start >= end {
		return
	}
	cursor := start
	var batch outputReadBatch
	valid := true
	for _, span := range timing.spans {
		if span.end <= start {
			continue
		}
		if span.start >= end {
			break
		}
		if span.start > cursor {
			valid = false
			break
		}
		if batch.id == 0 {
			batch = span.batch
		}
		if span.batch.id != batch.id {
			valid = false
		}
		cursor = min(end, span.end)
	}
	if valid && cursor == end && batch.id != 0 && !batch.at.IsZero() {
		timing.frame, timing.paired = batch, true
	}
	// Discard consumed ranges, including decoder read-ahead only up to this frame.
	consumed := 0
	for consumed < len(timing.spans) && timing.spans[consumed].end <= end {
		consumed++
	}
	if consumed > 0 {
		copy(timing.spans, timing.spans[consumed:])
		timing.spans = timing.spans[:len(timing.spans)-consumed]
	}
	if len(timing.spans) != 0 && timing.spans[0].start < end {
		timing.spans[0].start = end
	}
}

func (timing *outputDecoderTiming) stopMapping() {
	if timing != nil {
		timing.mappingDone = true
		timing.spans = nil
	}
}

// Read only the small usage array already isolated by the existing frame parser.
// Incomplete usage elsewhere must neither reuse cached counts nor change errors.
func freshPGOutputCount(raw json.RawMessage) (int64, bool, *int64) {
	var values []json.RawMessage
	if isJSONNull(raw) || json.Unmarshal(raw, &values) != nil {
		return 0, false, nil
	}
	var reasoning *int64
	var thoughtCount int64
	if value := rawAt(values, 9); !isJSONNull(value) && json.Unmarshal(value, &thoughtCount) == nil && thoughtCount >= 0 {
		reasoning = &thoughtCount
	}
	var count int64
	if isJSONNull(rawAt(values, 1)) || json.Unmarshal(values[1], &count) != nil {
		return 0, false, reasoning
	}
	return count, true, reasoning
}

func (timing *outputDecoderTiming) observe(events []Event, count int64, known bool, reasoning *int64, err error) {
	if timing != nil {
		timing.window.observeFrame(events, count, known, reasoning, timing.frame, timing.paired, err)
	}
}

func (attempt *physicalAttempt) outputDecoder() *outputDecoderTiming {
	if attempt == nil || attempt.output == nil {
		return nil
	}
	return &outputDecoderTiming{window: attempt.output}
}

func (attempt *physicalAttempt) finishOutput(timing *outputDecoderTiming, err error, finished bool) {
	if attempt == nil {
		return
	}
	attempt.output.finish(err == nil && finished && timing != nil && timing.complete)
	// Publish the final metric before the output channel closes. The existing
	// physical attempt status/termination semantics remain authoritative.
	attempt.update(func(*GenerationAttempt) {})
}
