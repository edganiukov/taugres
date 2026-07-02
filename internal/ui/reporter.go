package ui

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Reporter renders sync progress. In the default mode it shows a single
// updating line (the latest message) on a terminal, and stays quiet on a
// non-terminal. In verbose mode it prints every message on its own line.
//
// Subprocess output (e.g. mise) is fed through Stream, which prefixes each line
// so its origin is clear. In default mode the latest such line updates the
// single spinner line; in verbose mode every line is printed.
//
// Reporter is safe for concurrent use, so parallel tool installs can report and
// stream through the same reporter; each line is emitted atomically.
type Reporter struct {
	w       io.Writer
	verbose bool
	sp      *Spinner

	mu      sync.Mutex
	started bool
	sinks   []*streamSink
}

// NewReporter returns a reporter writing to w.
func NewReporter(w io.Writer, verbose bool) *Reporter {
	r := &Reporter{w: w, verbose: verbose}
	if !verbose && isTerminal(w) {
		r.sp = NewSpinner(w)
	}

	return r
}

// Step reports a tau-level progress message.
func (r *Reporter) Step(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.show(msg)
}

// show routes a fully-formed line to verbose output or the spinner. Callers
// must hold r.mu.
func (r *Reporter) show(msg string) {
	if r.verbose {
		fmt.Fprintln(r.w, msg)
		return
	}
	if r.sp == nil {
		return // non-verbose, non-terminal: stay quiet
	}

	if !r.started {
		r.sp.Start(msg)
		r.started = true
		return
	}

	r.sp.Update(msg)
}

// Stream returns an io.Writer for a subprocess's output. In verbose mode each
// complete line is printed, prefixed. In default mode the output is dropped —
// tau's own Step messages are the only progress shown, keeping it uncluttered.
// Flush any trailing partial line via Done.
func (r *Reporter) Stream(prefix string) io.Writer {
	if !r.verbose {
		return io.Discard
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	s := &streamSink{r: r, prefix: prefix}
	r.sinks = append(r.sinks, s)

	return s
}

// Done flushes any partial streamed lines and clears the spinner.
func (r *Reporter) Done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sinks {
		s.flush()
	}

	if r.sp != nil && r.started {
		r.sp.Stop()
		r.started = false
	}
}

// streamSink splits writes into lines and forwards them, prefixed.
type streamSink struct {
	r      *Reporter
	prefix string
	buf    []byte
}

// Write is called by subprocess output goroutines; it locks the reporter so
// concurrent streams (e.g. pip and npm running in parallel) emit whole lines
// atomically without interleaving mid-line.
func (s *streamSink) Write(b []byte) (int, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.buf = append(s.buf, b...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}

		s.r.show(s.prefix + string(s.buf[:i]))
		s.buf = s.buf[i+1:]
	}

	return len(b), nil
}

// flush emits any trailing partial line. Callers must hold r.mu.
func (s *streamSink) flush() {
	if len(s.buf) == 0 {
		return
	}

	s.r.show(s.prefix + string(s.buf))
	s.buf = nil
}
