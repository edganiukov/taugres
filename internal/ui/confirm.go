package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// Confirm writes a yes/no prompt to out and reads the answer from in, returning
// true only on an explicit "y"/"yes" (case-insensitive). It appends " [y/N]: "
// to prompt, so No is the default: a nil reader, EOF, or any other input counts
// as No, letting a non-interactive caller decline rather than hang.
//
// The read runs in a goroutine and selects on ctx, so a cancelled context
// (Ctrl+C) unblocks the prompt even though a bare stdin read cannot be
// interrupted. On cancellation Confirm returns false; the caller can inspect
// ctx.Err() to tell an abort apart from a plain "no". The abandoned goroutine,
// still blocked on the read, ends when the process exits.
func Confirm(ctx context.Context, out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", prompt)
	if in == nil {
		return false
	}
	ch := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		ch <- line
	}()
	select {
	case <-ctx.Done():
		return false
	case line := <-ch:
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		default:
			return false
		}
	}
}
