package handler

import (
	"errors"
	"io"
)

// ErrNotFound is returned when a requested TFTP file is not found.
var ErrNotFound = errors.New("file not found")

// Handler responds to a TFTP read request.
type Handler interface {
	ServeTFTP(filename string, rf io.ReaderFrom) error
}

// HandlerFunc is an adapter to allow the use of ordinary functions as TFTP handlers.
type HandlerFunc func(filename string, rf io.ReaderFrom) error

// ServeTFTP calls f(filename, rf).
func (f HandlerFunc) ServeTFTP(filename string, rf io.ReaderFrom) error {
	return f(filename, rf)
}

// NotFoundHandler returns a handler that always returns ErrNotFound.
func NotFoundHandler() Handler {
	return HandlerFunc(func(_ string, _ io.ReaderFrom) error {
		return ErrNotFound
	})
}
