package kit

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/tidwall/gjson"
)

// SSESplitter reassembles raw upstream body chunks into complete SSE events.
// CPA's host stream reads are fixed-size, so events can straddle chunks.
type SSESplitter struct {
	buf []byte
}

var (
	crlf          = []byte("\r\n")
	lf            = []byte("\n")
	sseBoundary   = []byte("\n\n")
	sseDataPrefix = []byte("data:")
	sseEventField = []byte("event:")
	sseDone       = []byte("[DONE]")
)

// Push appends a chunk and returns every event it completes, without the
// terminating blank line and with line endings normalized to "\n".
func (s *SSESplitter) Push(chunk []byte) [][]byte {
	s.buf = append(s.buf, chunk...)
	if bytes.Contains(s.buf, crlf) {
		s.buf = bytes.ReplaceAll(s.buf, crlf, lf)
	}
	var events [][]byte
	for {
		idx := bytes.Index(s.buf, sseBoundary)
		if idx < 0 {
			break
		}
		if event := bytes.Trim(s.buf[:idx], "\n"); len(event) > 0 {
			events = append(events, bytes.Clone(event))
		}
		s.buf = s.buf[idx+len(sseBoundary):]
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
	return events
}

// Flush returns a trailing event that was not followed by a blank line.
func (s *SSESplitter) Flush() []byte {
	event := bytes.Trim(s.buf, "\r\n")
	s.buf = nil
	if len(event) == 0 {
		return nil
	}
	return bytes.Clone(event)
}

// SSEFields extracts the event name and the joined data payload of one event.
func SSEFields(event []byte) (name string, data []byte) {
	var dataLines [][]byte
	for _, line := range bytes.Split(event, lf) {
		switch {
		case bytes.HasPrefix(line, sseDataPrefix):
			value := line[len(sseDataPrefix):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			dataLines = append(dataLines, value)
		case bytes.HasPrefix(line, sseEventField):
			name = string(bytes.TrimSpace(line[len(sseEventField):]))
		}
	}
	if len(dataLines) == 1 {
		return name, dataLines[0]
	}
	return name, bytes.Join(dataLines, lf)
}

// SSEFrame renders an event as a complete SSE frame for FormatClaude and
// FormatResponses streams.
func SSEFrame(name string, data []byte) []byte {
	frame := make([]byte, 0, len(name)+len(data)+16)
	if name != "" {
		frame = append(frame, "event: "...)
		frame = append(frame, name...)
		frame = append(frame, '\n')
	}
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	return append(frame, '\n', '\n')
}

// eventShaper converts upstream SSE events into the chunk form CPA's handlers
// expect: Chat Completions handlers add "data: " framing and [DONE] themselves,
// while Anthropic and Responses handlers write complete SSE frames.
type eventShaper struct {
	format       Format
	providerName string
	policy       ErrorPolicy
	splitter     SSESplitter
}

func (s *eventShaper) push(chunk []byte) ([][]byte, *pluginabi.Error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	return s.shapeAll(s.splitter.Push(chunk))
}

func (s *eventShaper) finish() ([][]byte, *pluginabi.Error) {
	tail := s.splitter.Flush()
	if tail == nil {
		return nil, nil
	}
	return s.shapeAll([][]byte{tail})
}

func (s *eventShaper) shapeAll(events [][]byte) ([][]byte, *pluginabi.Error) {
	out := make([][]byte, 0, len(events))
	for _, event := range events {
		name, data := SSEFields(event)
		trimmed := bytes.TrimSpace(data)
		if s.format == FormatChat {
			if len(trimmed) == 0 || bytes.Equal(trimmed, sseDone) {
				continue
			}
			if gjson.GetBytes(trimmed, "error").Exists() && !gjson.GetBytes(trimmed, "choices").Exists() {
				return out, s.policy.UpstreamFailure(s.providerName, http.StatusOK, nil, trimmed)
			}
			out = append(out, trimmed)
			continue
		}
		if name == "" && len(trimmed) == 0 {
			continue // comment-only keep-alive (": ping")
		}
		if name == "error" {
			return out, s.policy.UpstreamFailure(s.providerName, http.StatusOK, nil, trimmed)
		}
		frame := make([]byte, 0, len(event)+2)
		frame = append(frame, event...)
		out = append(out, append(frame, '\n', '\n'))
	}
	return out, nil
}

// sseStream adapts an upstream SSE body to Stream.Next.
type sseStream struct {
	upstream HTTPStream
	shaper   *eventShaper
	pending  [][]byte
	end      error
	provider string
}

func newSSEStream(upstream HTTPStream, shaper *eventShaper, first []byte, errFirst error) *sseStream {
	s := &sseStream{upstream: upstream, shaper: shaper, provider: shaper.providerName}
	s.feed(first, errFirst)
	return s
}

func (s *sseStream) feed(chunk []byte, errRead error) {
	events, failure := s.shaper.push(chunk)
	s.pending = append(s.pending, events...)
	switch {
	case failure != nil:
		s.end = failure
	case errRead == nil:
	case errors.Is(errRead, io.EOF):
		tail, tailFailure := s.shaper.finish()
		s.pending = append(s.pending, tail...)
		if tailFailure != nil {
			s.end = tailFailure
		} else {
			s.end = io.EOF
		}
	default:
		s.end = TransportFailure(s.provider, errRead)
	}
}

func (s *sseStream) next() ([]byte, error) {
	for len(s.pending) == 0 {
		if s.end != nil {
			return nil, s.end
		}
		s.feed(s.upstream.Next())
	}
	event := s.pending[0]
	s.pending = s.pending[1:]
	return event, nil
}
