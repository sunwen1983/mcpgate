package backend

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLine(t *testing.T) {
	t.Run("short line", func(t *testing.T) {
		line, err := readLine(bufio.NewReader(strings.NewReader("{\"jsonrpc\":\"2.0\"}\nnext\n")), 100)
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if string(line) != "{\"jsonrpc\":\"2.0\"}\n" {
			t.Errorf("line = %q", line)
		}
	})

	t.Run("line spanning reader buffer", func(t *testing.T) {
		// Longer than bufio's default 4096-byte buffer, under the limit:
		// must come back whole via fragment accumulation.
		want := strings.Repeat("a", 10000) + "\n"
		line, err := readLine(bufio.NewReader(strings.NewReader(want)), 1<<20)
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if string(line) != want {
			t.Errorf("line length = %d, want %d", len(line), len(want))
		}
	})

	t.Run("line at exactly the limit", func(t *testing.T) {
		want := strings.Repeat("b", 99) + "\n" // 100 bytes total
		line, err := readLine(bufio.NewReader(strings.NewReader(want)), 100)
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if string(line) != want {
			t.Errorf("line = %q", line)
		}
	})

	t.Run("line over the limit", func(t *testing.T) {
		// An endless line must be cut off at the limit, not read whole.
		src := strings.Repeat("c", 100000) + "\n"
		_, err := readLine(bufio.NewReader(strings.NewReader(src)), 100)
		if !errors.Is(err, errLineTooLong) {
			t.Fatalf("err = %v, want errLineTooLong", err)
		}
	})

	t.Run("eof without newline", func(t *testing.T) {
		line, err := readLine(bufio.NewReader(strings.NewReader("partial")), 100)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if string(line) != "partial" {
			t.Errorf("line = %q", line)
		}
	})
}
