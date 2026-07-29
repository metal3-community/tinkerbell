package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	ipmi "github.com/bougou/go-ipmi"
	"github.com/gliderlabs/ssh"
	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/data"
)

// State is the internal State needed to track multiple sessions
// and provide a way to share stdin between sessions.
type State struct {
	// initialClosed is a channel to signal to all connected sessions that the initial session has closed.
	initialClosed chan struct{}
	// wg is used to wait for all connected sessions to close before closing
	// the main session and removing state from the global state.
	wg sync.WaitGroup
	// additionalSessions is a map of all the additional sessions connected to the initial session.
	additionalSessions atomic.Int32
	// multiwriter allows multiple sessions to have the same stdout.
	multiwriter *MultiWriter
	// stdin is the shared stdin for all connected sessions. A multiwriter is not needed here because
	// each ssh session has its own stdin.
	stdin io.Writer
}

// Handler returns a function that can be used as the ssh.Handler for the gliderlabs/ssh server.
func Handler(log logr.Logger, globalState *KeyValueStore) func(s ssh.Session) {
	return func(s ssh.Session) {
		if st, found := globalState.Get(s.User()); found {
			additionalSession(log, s, st)
			return
		}
		initialSession(log, s, globalState)
	}
}

// initialSession is the handler for the initial or first session connected to the ssh server for a specific host.
func initialSession(log logr.Logger, s ssh.Session, globalState *KeyValueStore) {
	log = log.WithValues("user", s.User(), "sessionName", s.User(), "mainSession", true)
	log.V(2).Info("new session")

	bmc, ok := s.Context().Value(BMCDataKey).(data.BMCMachine)
	if !ok {
		log.V(2).Info("error getting bmc info, exiting session")
		if err := s.Exit(1); err != nil {
			log.Error(err, "error closing session")
		}
		return
	}

	port := bmc.Port
	if port == 0 {
		port = 623
	}

	client, err := ipmi.NewClient(bmc.Host, port, bmc.User, bmc.Pass)
	if err != nil {
		log.Error(err, "error creating IPMI client")
		if err := s.Exit(2); err != nil {
			log.Error(err, "error closing session")
		}
		return
	}

	if bmc.CipherSuite != "" {
		id, parseErr := parseCipherSuiteID(bmc.CipherSuite)
		if parseErr != nil {
			log.Error(parseErr, "invalid cipher suite, using default")
		} else {
			client.WithCipherSuiteID(id)
		}
	}

	if err := client.Connect(s.Context()); err != nil {
		log.Error(err, "error connecting to BMC")
		if err := s.Exit(2); err != nil {
			log.Error(err, "error closing session")
		}
		return
	}
	defer client.Close(context.Background())

	// pr/pw aggregate stdin from all sessions; SOLActivate reads from pr.
	pr, pw := io.Pipe()
	defer pr.Close()

	mw := NewMultiWriter()
	mw.Add(s)

	globalState.Set(s.User(), &State{
		wg:            sync.WaitGroup{},
		initialClosed: make(chan struct{}),
		multiwriter:   mw,
		stdin:         pw,
	})

	go func() {
		_, _ = io.Copy(pw, s)
	}()

	solOpts := &ipmi.SOLActivateOptions{
		OnActivated: func(_ uint8, _ io.Reader, out io.Writer, _ *ipmi.ActivatePayloadResponse) {
			_, _ = io.WriteString(out, "SOL session active. Use ~. to disconnect.\n")
		},
		OnDeactivated: func(_ uint8, _ io.Reader, out io.Writer, _ *ipmi.ActivatePayloadResponse) {
			_, _ = io.WriteString(out, "\r\nSOL session closed.\n")
		},
	}

	if err := client.SOLActivate(s.Context(), pr, mw, solOpts); err != nil && !errors.Is(err, context.Canceled) {
		log.Error(err, "SOL session error")
	}

	v, ok := globalState.Get(s.User())
	if ok && v.additionalSessions.Load() > 0 {
		if st, ok := globalState.Get(s.User()); ok {
			st.initialClosed <- struct{}{}
		}
	}
	if ok {
		v.wg.Wait()
	}
	globalState.Delete(s.User())

	log.V(2).Info("session closed")
}

// additionalSession is the handler for all additional sessions connected to an initial session.
func additionalSession(log logr.Logger, s ssh.Session, st *State) {
	num := st.additionalSessions.Add(1)
	name := fmt.Sprintf("%v-%v", s.User(), num)
	log = log.WithValues("sessionName", name, "user", s.User(), "mainSession", false)
	log.V(2).Info("connecting to an existing session", "user", s.User())
	st.wg.Add(1)
	defer st.wg.Done()
	st.multiwriter.Add(s) // stdout
	defer func() {
		st.multiwriter.Remove(s)
		st.additionalSessions.Add(-1)
	}()
	exit := make(chan struct{})
	escapeReader, escapeWriter := io.Pipe()
	mw := io.MultiWriter(st.stdin, escapeWriter)
	// watch for escape sequences
	// escape sequence is ~.
	// if ~. is detected, close the session
	go func() {
		for {
			b := make([]byte, 1)
			_, err := escapeReader.Read(b)
			if err != nil {
				log.Error(err, "error reading escape sequence")
				return
			}
			if b[0] == '~' {
				_, err := escapeReader.Read(b)
				if err != nil {
					log.Error(err, "error reading escape sequence")
					return
				}
				if b[0] == '.' {
					exit <- struct{}{}
					return
				}
			}
		}
	}()
	go func() {
		if _, err := io.Copy(mw, s); err != nil { // stdin
			log.Error(err, "error copying stdin")
		}
	}()
	select {
	case <-st.initialClosed:
		log.V(2).Info("closing additional session", "reason", "the main session has closed")
		if err := s.Exit(0); err != nil {
			log.Error(err, "error closing session")
		}
		return
	case <-s.Context().Done():
		log.V(2).Info("closing additional session", "reason", "context done")
		if err := s.Exit(0); err != nil && !errors.Is(err, io.EOF) {
			log.Error(err, "error closing session")
		}
		return
	case <-exit:
		log.V(2).Info("closing additional session", "reason", "escape sequence detected")
		if err := s.Exit(0); err != nil {
			log.Error(err, "error closing session")
		}
		return
	}
}

// parseCipherSuiteID parses a numeric string into an ipmi.CipherSuiteID.
func parseCipherSuiteID(s string) (ipmi.CipherSuiteID, error) {
	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return 0, fmt.Errorf("cipher suite %q must be a decimal integer: %w", s, err)
	}
	return ipmi.CipherSuiteID(n), nil
}
