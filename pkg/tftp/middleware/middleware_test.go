package middleware

import (
	"errors"
	"io"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
)

type mockReaderFrom struct{}

func (m *mockReaderFrom) ReadFrom(io.Reader) (int64, error) { return 0, nil }

func TestLogging_Success(t *testing.T) {
	sink := &spySink{}
	logger := logr.New(sink)

	inner := handler.HandlerFunc(func(_ string, _ io.ReaderFrom) error {
		return nil
	})

	mw := Logging(logger)
	wrapped := mw(inner)

	if err := wrapped.ServeTFTP("test.bin", &mockReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sink.infoCalled {
		t.Error("expected Info to be called for successful request")
	}
}

func TestLogging_Failure(t *testing.T) {
	sink := &spySink{}
	logger := logr.New(sink)

	wantErr := errors.New("test failure")
	inner := handler.HandlerFunc(func(_ string, _ io.ReaderFrom) error {
		return wantErr
	})

	mw := Logging(logger)
	wrapped := mw(inner)

	err := wrapped.ServeTFTP("test.bin", &mockReaderFrom{})
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want %v", err, wantErr)
	}
	if !sink.errorCalled {
		t.Error("expected Error to be called for failed request")
	}
}

// spySink is a minimal logr.LogSink that records whether Info/Error was called.
type spySink struct {
	infoCalled  bool
	errorCalled bool
}

func (s *spySink) Init(logr.RuntimeInfo)             {}
func (s *spySink) Enabled(int) bool                  { return true }
func (s *spySink) Info(_ int, _ string, _ ...any)    { s.infoCalled = true }
func (s *spySink) Error(_ error, _ string, _ ...any) { s.errorCalled = true }
func (s *spySink) WithValues(_ ...any) logr.LogSink  { return s }
func (s *spySink) WithName(_ string) logr.LogSink    { return s }
