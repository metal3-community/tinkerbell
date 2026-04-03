package server

import (
	"fmt"
	"io"
	"regexp"
	"sync"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
)

// Route represents a TFTP route with its regex pattern, description, and handler.
type Route struct {
	Pattern     string          `json:"pattern"`
	Description string          `json:"description"`
	Handler     handler.Handler `json:"-"`
}

// Routes is a collection of Route objects that can be registered with a TFTP server.
type Routes []Route

// Register adds a new route to the Routes collection for later registration with a ServeMux.
func (rs *Routes) Register(pattern string, h handler.Handler, desc string) {
	if desc == "" {
		desc = "No description provided"
	}
	*rs = append(*rs, Route{
		Pattern:     pattern,
		Description: desc,
		Handler:     h,
	})
}

// Mux builds a ServeMux from the registered routes.
func (rs *Routes) Mux(log logr.Logger) *ServeMux {
	mux := NewServeMux()
	mux.log = log
	for _, route := range *rs {
		mux.Handle(route.Pattern, route.Handler)
	}
	return mux
}

// patternHandler holds a compiled regex pattern and its associated handler.
type patternHandler struct {
	pattern *regexp.Regexp
	handler handler.Handler
}

// ServeMux is a TFTP request multiplexer that matches filenames against
// registered regex patterns and routes them to the appropriate handler.
type ServeMux struct {
	defaultHandler handler.Handler
	mu             sync.RWMutex
	patterns       []patternHandler
	log            logr.Logger
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux {
	return &ServeMux{}
}

// SetLogger sets the logger for the ServeMux.
func (mux *ServeMux) SetLogger(log logr.Logger) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	mux.log = log
}

// Handle registers the handler for the given regex pattern.
// If a pattern is malformed, Handle panics.
func (mux *ServeMux) Handle(pattern string, h handler.Handler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()

	regex, err := regexp.Compile(pattern)
	if err != nil {
		panic("tftp: invalid pattern " + pattern + ": " + err.Error())
	}

	mux.patterns = append(mux.patterns, patternHandler{
		pattern: regex,
		handler: h,
	})
}

// HandleFunc registers the handler function for the given regex pattern.
func (mux *ServeMux) HandleFunc(pattern string, h func(filename string, rf io.ReaderFrom) error) {
	mux.Handle(pattern, handler.HandlerFunc(h))
}

// SetDefaultHandler sets the handler used when no pattern matches.
func (mux *ServeMux) SetDefaultHandler(h handler.Handler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	mux.defaultHandler = h
}

func (mux *ServeMux) findHandler(filename string) (handler.Handler, error) {
	mux.mu.RLock()
	defer mux.mu.RUnlock()

	for _, ph := range mux.patterns {
		if ph.pattern.MatchString(filename) {
			mux.log.V(2).Info("tftp request matched pattern",
				"filename", filename,
				"pattern", ph.pattern.String())
			return ph.handler, nil
		}
	}
	return nil, fmt.Errorf("no handler found for filename: %s", filename)
}

// ServeTFTP dispatches the request to the handler whose pattern matches
// the request filename. If no handler is found and a default handler is set, it
// is used. Otherwise ErrNotFound is returned.
func (mux *ServeMux) ServeTFTP(filename string, rf io.ReaderFrom) error {
	matchedHandler, err := mux.findHandler(filename)
	if err != nil {
		if mux.defaultHandler != nil {
			mux.log.V(2).Info("using default tftp handler for filename",
				"filename", filename)
			return mux.defaultHandler.ServeTFTP(filename, rf)
		}
		mux.log.Info("no tftp handler found for filename", "filename", filename)
		return handler.ErrNotFound
	}

	if matchedHandler != nil {
		mux.log.V(2).Info("tftp request matched pattern",
			"filename", filename)
		return matchedHandler.ServeTFTP(filename, rf)
	}

	mux.log.Info("no tftp handler found for filename", "filename", filename)
	return handler.ErrNotFound
}
