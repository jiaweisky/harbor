package bridge

import (
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	// flushInterval bounds how long buffered text may wait before being emitted.
	flushInterval = 80 * time.Millisecond
	// flushBytes bounds how much text accumulates before being emitted.
	flushBytes = 1024
)

// textSink buffers text deltas and emits them as a single artifact, appending
// later flushes to the same artifact id. It rate-limits emissions to avoid
// flooding a streaming client.
type textSink struct {
	ec         *a2asrv.ExecutorContext
	artifactID a2a.ArtifactID
	buf        strings.Builder
	emit       func(a2a.Event) bool
}

func newTextSink(ec *a2asrv.ExecutorContext, emit func(a2a.Event) bool) *textSink {
	return &textSink{ec: ec, emit: emit}
}

// Write appends text, flushing once the buffer reaches flushBytes. It returns
// false when the consumer has gone away.
func (s *textSink) Write(text string) bool {
	s.buf.WriteString(text)
	if s.buf.Len() < flushBytes {
		return true
	}
	return s.Flush()
}

// Flush emits any buffered text. It returns false when the consumer is gone.
func (s *textSink) Flush() bool {
	if s.buf.Len() == 0 {
		return true
	}
	text := s.buf.String()
	s.buf.Reset()
	return s.emit(s.event(text))
}

// event builds the artifact event for this chunk, starting a new artifact on
// the first flush and appending to it thereafter.
func (s *textSink) event(text string) a2a.Event {
	if s.artifactID == "" {
		evt := a2a.NewArtifactEvent(s.ec, a2a.NewTextPart(text))
		if evt.Artifact != nil {
			s.artifactID = evt.Artifact.ID
		}
		return evt
	}
	return a2a.NewArtifactUpdateEvent(s.ec, s.artifactID, a2a.NewTextPart(text))
}
