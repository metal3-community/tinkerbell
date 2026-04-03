package handler

import (
	"errors"
	"io"
	"testing"
)

type mockReaderFrom struct {
	n   int64
	err error
}

func (m *mockReaderFrom) ReadFrom(io.Reader) (int64, error) {
	return m.n, m.err
}

func TestHandlerFunc_ServeTFTP(t *testing.T) {
	called := false
	h := HandlerFunc(func(filename string, _ io.ReaderFrom) error {
		called = true
		if filename != "test.bin" {
			t.Errorf("filename = %q, want %q", filename, "test.bin")
		}
		return nil
	})

	if err := h.ServeTFTP("test.bin", &mockReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler function was not called")
	}
}

func TestHandlerFunc_ReturnsError(t *testing.T) {
	want := errors.New("test error")
	h := HandlerFunc(func(_ string, _ io.ReaderFrom) error {
		return want
	})

	got := h.ServeTFTP("file.bin", &mockReaderFrom{})
	if !errors.Is(got, want) {
		t.Errorf("error = %v, want %v", got, want)
	}
}

func TestNotFoundHandler(t *testing.T) {
	h := NotFoundHandler()
	err := h.ServeTFTP("missing.bin", &mockReaderFrom{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want %v", err, ErrNotFound)
	}
}
