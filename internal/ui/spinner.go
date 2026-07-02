// Package ui provides small terminal helpers such as a progress spinner.
package ui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Spinner shows an animated progress indicator on a terminal. When the target
// writer is not a terminal (pipes, CI, tests) it degrades to a single plain
// line per step, so output stays clean and deterministic.
type Spinner struct {
	w      io.Writer
	tty    bool
	frames []string
	delay  time.Duration

	mu   sync.Mutex
	msg  string
	stop chan struct{}
	done chan struct{}
}

// NewSpinner returns a spinner writing to w.
func NewSpinner(w io.Writer) *Spinner {
	return &Spinner{
		w:      w,
		tty:    isTerminal(w),
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		delay:  100 * time.Millisecond,
	}
}

// Start begins a step with the given message. On a TTY it animates until
// Success or Stop is called; otherwise it prints the message once.
func (s *Spinner) Start(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()

	if !s.tty {
		fmt.Fprintln(s.w, msg)
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.draw(s.frames[0])
	go s.loop()
}

// Update changes the message shown next to the spinner. On a TTY the animation
// loop redraws it on the next frame.
func (s *Spinner) Update(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
}

// Success stops the animation and prints a final success line (TTY only; the
// non-TTY line was already printed by Start).
func (s *Spinner) Success(msg string) {
	if !s.tty {
		return
	}
	s.stopAnim()
	fmt.Fprintf(s.w, "\r\033[K\033[32m✓\033[0m %s\n", msg)
}

// Stop halts the animation without printing a success line.
func (s *Spinner) Stop() {
	if !s.tty {
		return
	}
	s.stopAnim()
	fmt.Fprint(s.w, "\r\033[K")
}

func (s *Spinner) loop() {
	defer close(s.done)
	t := time.NewTicker(s.delay)
	defer t.Stop()
	i := 1
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.draw(s.frames[i%len(s.frames)])
			i++
		}
	}
}

func (s *Spinner) draw(frame string) {
	s.mu.Lock()
	msg := s.msg
	s.mu.Unlock()
	// Truncate so "<frame> <msg>" never exceeds the terminal width; otherwise it
	// wraps to a new row and the \r-based redraw can't overwrite it, spilling a
	// new line per frame. \033[K clears any leftover from a longer prior message.
	fmt.Fprintf(s.w, "\r\033[2m%s\033[0m %s\033[K", frame, truncate(msg, s.width()-2))
}

// width returns the terminal column count, defaulting to 80.
func (s *Spinner) width() int {
	if f, ok := s.w.(*os.File); ok {
		if w := terminalWidth(f); w > 0 {
			return w
		}
	}
	return 80
}

// truncate shortens s to at most maxCols display columns (by rune count),
// replacing the tail with an ellipsis when cut.
func truncate(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxCols {
		return s
	}
	if maxCols == 1 {
		return "…"
	}
	return string(r[:maxCols-1]) + "…"
}

func (s *Spinner) stopAnim() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
}

// isTerminal reports whether w is a character device (a terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
