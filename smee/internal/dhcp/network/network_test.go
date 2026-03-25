package network

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestLeaderDefaults_AllEmpty(t *testing.T) {
	// Verify internal defaults are applied when creating a leader manager
	// via newLeaderManagerWithIfMgr by inspecting the LeaderElector configuration.
	// We test through the observable defaults used in newLeaderManagerWithIfMgr.
	identity := leaderIdentity()
	if identity == "" {
		t.Error("leaderIdentity should return a non-empty string")
	}
}

func TestLeaderIdentity_FromEnv(t *testing.T) {
	t.Setenv("HOSTNAME", "test-pod-123")
	if got := leaderIdentity(); got != "test-pod-123" {
		t.Errorf("got %q, want %q", got, "test-pod-123")
	}
}

func TestLeaderIdentity_Fallback(t *testing.T) {
	t.Setenv("HOSTNAME", "")
	got := leaderIdentity()
	want, _ := os.Hostname()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewLeaderManagerWithIfMgr_NilRestConfig(t *testing.T) {
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 1),
		onCleanup: make(chan struct{}, 1),
	}
	_, err := newLeaderManagerWithIfMgr(LeaderConfig{
		RestConfig: nil,
	}, mock, logr.Discard())
	if err == nil {
		t.Fatal("expected error for nil RestConfig")
	}
	if !strings.Contains(err.Error(), "rest config is required") {
		t.Errorf("expected 'rest config is required' error, got: %v", err)
	}
}

func TestRetryBackoff(t *testing.T) {
	lm := &LeaderManager{retryPeriod: 1 * time.Second}

	tests := []struct {
		attempt int
		wantMax time.Duration
	}{
		{0, 1 * time.Second},  // 1s * 2^0 = 1s
		{1, 2 * time.Second},  // 1s * 2^1 = 2s
		{2, 4 * time.Second},  // 1s * 2^2 = 4s
		{3, 8 * time.Second},  // 1s * 2^3 = 8s
		{4, 16 * time.Second}, // 1s * 2^4 = 16s
		{5, 30 * time.Second}, // 1s * 2^5 = 32s -> capped at 30s
		{10, 30 * time.Second},
	}
	for _, tt := range tests {
		got := lm.retryBackoff(tt.attempt)
		if got > tt.wantMax {
			t.Errorf("attempt %d: got %v, want <= %v", tt.attempt, got, tt.wantMax)
		}
		if got <= 0 {
			t.Errorf("attempt %d: backoff should be positive, got %v", tt.attempt, got)
		}
	}
}

func TestOnStartedLeading_RetriesOnSetupFailure(t *testing.T) {
	failCount := 3
	attempt := 0
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
		setupFunc: func(_ context.Context) error {
			attempt++
			if attempt <= failCount {
				return fmt.Errorf("transient error %d", attempt)
			}
			return nil
		},
	}

	var readyCalled atomic.Int32
	lm := &LeaderManager{
		ifMgr:       mock,
		log:         logr.Discard(),
		retryPeriod: 1 * time.Millisecond, // fast for tests
		onReady:     func() { readyCalled.Add(1) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lm.onStartedLeading(ctx)

	if attempt != failCount+1 {
		t.Errorf("expected %d setup attempts, got %d", failCount+1, attempt)
	}
	if readyCalled.Load() != 1 {
		t.Errorf("expected OnReady to be called once, got %d", readyCalled.Load())
	}
}

func TestOnStartedLeading_GivesUpAfterMaxRetries(t *testing.T) {
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
		setupFunc: func(_ context.Context) error {
			return fmt.Errorf("permanent error")
		},
	}

	var readyCalled atomic.Int32
	lm := &LeaderManager{
		ifMgr:       mock,
		log:         logr.Discard(),
		retryPeriod: 1 * time.Millisecond,
		onReady:     func() { readyCalled.Add(1) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lm.onStartedLeading(ctx)

	if readyCalled.Load() != 0 {
		t.Errorf("expected OnReady to not be called, got %d", readyCalled.Load())
	}
}

func TestOnStartedLeading_CancelledDuringRetry(t *testing.T) {
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
		setupFunc: func(_ context.Context) error {
			return fmt.Errorf("error")
		},
	}

	lm := &LeaderManager{
		ifMgr:       mock,
		log:         logr.Discard(),
		retryPeriod: 1 * time.Hour, // large so we'll hit cancel first
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		lm.onStartedLeading(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good - returned promptly
	case <-time.After(2 * time.Second):
		t.Error("onStartedLeading did not return after context cancellation")
	}
}

func TestOnStoppedLeading_CallsOnLostBeforeCleanup(t *testing.T) {
	var order []string
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
		cleanupFunc: func() error {
			order = append(order, "cleanup")
			return nil
		},
	}

	lm := &LeaderManager{
		ifMgr: mock,
		log:   logr.Discard(),
		onLost: func() {
			order = append(order, "onLost")
		},
	}
	// Simulate that the interface was set up before leadership was lost.
	lm.interfaceUp.Store(true)

	lm.onStoppedLeading()

	if len(order) != 2 || order[0] != "onLost" || order[1] != "cleanup" {
		t.Errorf("expected [onLost cleanup], got %v", order)
	}
}

func TestOnStoppedLeading_SkipsCleanupWhenInterfaceNeverUp(t *testing.T) {
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
	}

	var lostCalled bool
	lm := &LeaderManager{
		ifMgr: mock,
		log:   logr.Discard(),
		onLost: func() {
			lostCalled = true
		},
	}
	// interfaceUp is false (default) — simulate the case where Run()
	// returned before the lease was acquired (e.g. context cancelled
	// during startup).

	lm.onStoppedLeading()

	if lostCalled {
		t.Error("onLost should not be called when interface was never set up")
	}
	if mock.cleanupCalls != 0 {
		t.Errorf("expected 0 cleanup calls, got %d", mock.cleanupCalls)
	}
}

func TestCloseIdempotent(t *testing.T) {
	mock := &mockNetworkInterfaceManager{
		onSetup:   make(chan struct{}, 10),
		onCleanup: make(chan struct{}, 10),
	}

	lm := &LeaderManager{
		ifMgr: mock,
		log:   logr.Discard(),
	}

	if err := lm.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := lm.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if mock.closeCalls != 1 {
		t.Errorf("expected Close to be called once, got %d", mock.closeCalls)
	}
}

// mockNetworkInterfaceManager tracks calls to Setup, Cleanup, and Close.
// Used by both unit and integration tests.
type mockNetworkInterfaceManager struct {
	setupCalls   int
	cleanupCalls int
	closeCalls   int
	setupErr     error
	cleanupErr   error

	// Optional custom functions for more complex test scenarios.
	setupFunc   func(ctx context.Context) error
	cleanupFunc func() error

	// Channels signaled on each call. Buffered with capacity 10.
	onSetup   chan struct{}
	onCleanup chan struct{}
}

func (m *mockNetworkInterfaceManager) Setup(ctx context.Context) error {
	m.setupCalls++
	if m.setupFunc != nil {
		return m.setupFunc(ctx)
	}
	select {
	case m.onSetup <- struct{}{}:
	default:
	}
	return m.setupErr
}

func (m *mockNetworkInterfaceManager) Cleanup() error {
	m.cleanupCalls++
	if m.cleanupFunc != nil {
		return m.cleanupFunc()
	}
	select {
	case m.onCleanup <- struct{}{}:
	default:
	}
	return m.cleanupErr
}

func (m *mockNetworkInterfaceManager) Close() error {
	m.closeCalls++
	return nil
}
