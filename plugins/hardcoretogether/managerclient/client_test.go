package managerclient_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/internal/mockmanager"
	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/managerclient"
)

// newClient applies configure (setting any On* callback fields) before
// starting Run, never after: those fields are plain unsynchronized struct
// fields (like the real Client is meant to be configured once via plugin.go
// before Run starts), so setting one concurrently with Run already reading
// it is a data race, not just bad style.
func newClient(t *testing.T, addr string, configure ...func(*managerclient.Client)) *managerclient.Client {
	t.Helper()
	c := managerclient.New(addr, logr.Discard())
	for _, fn := range configure {
		fn(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	waitConnected(t, c)
	return c
}

func waitConnected(t *testing.T, c *managerclient.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client never connected to mock manager")
}

func TestQueryState(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "state-query" {
			t.Errorf("unexpected message type %q", msg.Type)
			return nil
		}
		return []mockmanager.Message{{Type: "state-response", State: "ready", Running: "true"}}
	})

	c := newClient(t, srv.Addr)

	state, running, err := c.QueryState(context.Background())
	if err != nil {
		t.Fatalf("QueryState: %v", err)
	}
	if state != managerclient.StateReady || running != managerclient.RunningTrue {
		t.Fatalf("got state=%q running=%q, want ready/true", state, running)
	}
}

func TestStartRejected(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		return []mockmanager.Message{{Type: "start-rejected", Reason: "挑戦が進行中です"}}
	})

	type rejection struct{ requestID, reason string }
	rejected := make(chan rejection, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminRejected = func(_ context.Context, requestID, reason string) {
			rejected <- rejection{requestID, reason}
		}
	})

	requestID := managerclient.NewRequestID()
	if err := c.Start(context.Background(), requestID, false, "Steve"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case rej := <-rejected:
		if rej.reason != "挑戦が進行中です" {
			t.Fatalf("OnAdminRejected reason = %q, want 挑戦が進行中です", rej.reason)
		}
		if rej.requestID != requestID {
			t.Fatalf("OnAdminRejected requestID = %q, want %q", rej.requestID, requestID)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminRejected was not called")
	}

	recv := srv.Received()
	if len(recv) != 1 || recv[0].Clean || recv[0].RequestedBy != "Steve" || recv[0].RequestID != requestID {
		t.Fatalf("manager received %+v, want single start{clean:false,requestedBy:Steve,requestId:%s}", recv, requestID)
	}
}

// TestStartAcceptedTriggersEvacuateHandshake covers clean:true accepted
// against a running process: Start returns as soon as the request is sent
// (it never waits for Manager's reply), and the evacuate handshake happens
// independently via OnEvacuateRequest.
func TestStartAcceptedTriggersEvacuateHandshake(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		return []mockmanager.Message{{Type: "evacuate-request", Reason: "force-reset"}}
	})

	evacuated := make(chan string, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnEvacuateRequest = func(_ context.Context, reason string) {
			evacuated <- reason
		}
	})

	requestID := managerclient.NewRequestID()
	if err := c.Start(context.Background(), requestID, true, "Steve"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case reason := <-evacuated:
		if reason != "force-reset" {
			t.Fatalf("OnEvacuateRequest reason = %q, want force-reset", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("OnEvacuateRequest was not called")
	}

	// The client must send evacuate-complete on its own once the callback
	// returns, echoing back Manager's evacuate-request requestId.
	deadline := time.Now().Add(time.Second)
	for {
		recv := srv.Received()
		if len(recv) >= 2 {
			if recv[1].Type != "evacuate-complete" || recv[1].RequestID != requestID {
				t.Fatalf("second message = %+v, want evacuate-complete{requestId:%s}", recv[1], requestID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager never received evacuate-complete, got %+v", recv)
		}
		time.Sleep(5 * time.Millisecond)
	}

	recvFirst := srv.Received()[0]
	if !recvFirst.Clean || recvFirst.RequestedBy != "Steve" || recvFirst.RequestID != requestID {
		t.Fatalf("manager received %+v, want start{clean:true,requestedBy:Steve,requestId:%s}", recvFirst, requestID)
	}
}

// TestAdminCompletedOnHardcoreReady covers the case where Start/Load are
// accepted against an already-stopped process: Manager sends no immediate
// acknowledgement at all (docs/protocol-gate-manager.md 3.2節), so
// hardcore-ready is the only signal that ever arrives, and it must reach
// both OnAdminCompleted (the pending command's own completion notice,
// carrying the original requestID) and OnHardcoreReady (the lobby-wide
// auto-transfer) independently.
func TestAdminCompletedOnHardcoreReady(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		if msg.Clean {
			t.Errorf("start message had clean=true, want false")
		}
		return nil
	})

	completed := make(chan string, 1)
	ready := make(chan struct{}, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminCompleted = func(_ context.Context, requestID string) { completed <- requestID }
		c.OnHardcoreReady = func(context.Context) { ready <- struct{}{} }
	})

	requestID := managerclient.NewRequestID()
	if err := c.Start(context.Background(), requestID, false, "Steve"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for len(srv.Received()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("manager never received start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	srv.Push(mockmanager.Message{Type: "hardcore-ready", RequestID: requestID})

	select {
	case got := <-completed:
		if got != requestID {
			t.Fatalf("OnAdminCompleted requestID = %q, want %q", got, requestID)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminCompleted was not called")
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("OnHardcoreReady was not called")
	}
}

// TestAdminFailedAfterAcceptance covers start-failed (docs/protocol-gate-manager.md
// 3.5b節): an accepted request that later fails (e.g. the process crashed
// before becoming ready) delivers reason/recovered via OnAdminFailed,
// carrying the original requestID.
func TestAdminFailedAfterAcceptance(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })

	type failure struct {
		requestID, reason string
		recovered         bool
	}
	failed := make(chan failure, 1)
	newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminFailed = func(_ context.Context, requestID, reason string, recovered bool) {
			failed <- failure{requestID, reason, recovered}
		}
	})

	requestID := managerclient.NewRequestID()
	srv.Push(mockmanager.Message{
		Type:      "start-failed",
		RequestID: requestID,
		Reason:    "process exited before ready",
		Recovered: false,
	})

	select {
	case f := <-failed:
		if f.requestID != requestID || f.reason != "process exited before ready" || f.recovered {
			t.Fatalf("got %+v, want {requestID:%s reason:process exited before ready recovered:false}", f, requestID)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminFailed was not called")
	}
}

// TestConcurrentAdminRequestsAreRoutedIndependently guards against a
// regression of a real bug: without per-request correlation, two
// concurrent admin requests (e.g. /start from two different players) would
// share a single pending slot, so whichever request's outcome arrived
// second would silently overwrite/lose the first's. requestID
// (docs/protocol-gate-manager.md 1節) fixes this — each outcome must route
// back by its own requestID regardless of how many requests are in flight.
func TestConcurrentAdminRequestsAreRoutedIndependently(t *testing.T) {
	var mu sync.Mutex
	firstSeen := ""
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if firstSeen == "" {
			firstSeen = msg.RequestID
			return nil // accepted silently; completed later via a pushed hardcore-ready
		}
		return []mockmanager.Message{{Type: "start-rejected", Reason: "処理中です"}}
	})

	type rejection struct{ requestID, reason string }
	rejected := make(chan rejection, 2)
	completed := make(chan string, 2)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminRejected = func(_ context.Context, requestID, reason string) {
			rejected <- rejection{requestID, reason}
		}
		c.OnAdminCompleted = func(_ context.Context, requestID string) {
			completed <- requestID
		}
	})

	idA := managerclient.NewRequestID()
	if err := c.Start(context.Background(), idA, false, "Alice"); err != nil {
		t.Fatalf("Start (Alice): %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for len(srv.Received()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("manager never received Alice's start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	idB := managerclient.NewRequestID()
	if err := c.Start(context.Background(), idB, false, "Bob"); err != nil {
		t.Fatalf("Start (Bob): %v", err)
	}

	// Bob's request loses the race and must be rejected under his own
	// requestID, not Alice's.
	select {
	case rej := <-rejected:
		if rej.requestID != idB {
			t.Fatalf("rejected requestID = %q, want %q (Bob's)", rej.requestID, idB)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a rejection for Bob's start")
	}

	// Alice's request is still pending; completing it must route to her
	// requestID, even though Bob's request was sent (and rejected) in the
	// meantime.
	srv.Push(mockmanager.Message{Type: "hardcore-ready", RequestID: idA})
	select {
	case got := <-completed:
		if got != idA {
			t.Fatalf("completed requestID = %q, want %q (Alice's)", got, idA)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a completion for Alice's start")
	}
}

func TestLoadRejected(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "load" {
			return nil
		}
		return []mockmanager.Message{{Type: "load-rejected", Reason: "アーカイブ save1 は存在しません"}}
	})

	rejected := make(chan string, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminRejected = func(_ context.Context, _ string, reason string) { rejected <- reason }
	})

	requestID := managerclient.NewRequestID()
	if err := c.Load(context.Background(), requestID, "save1", false, "Steve"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	select {
	case reason := <-rejected:
		if reason != "アーカイブ save1 は存在しません" {
			t.Fatalf("OnAdminRejected reason = %q, want アーカイブ save1 は存在しません", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminRejected was not called")
	}

	recv := srv.Received()
	if len(recv) != 1 || recv[0].Name != "save1" || recv[0].RequestID != requestID {
		t.Fatalf("manager received %+v, want single load{name:save1,requestId:%s}", recv, requestID)
	}
}

func TestDeactivateRejected(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "deactivate" {
			return nil
		}
		return []mockmanager.Message{{Type: "deactivate-rejected", Reason: "既に停止しています"}}
	})

	rejected := make(chan string, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminRejected = func(_ context.Context, _ string, reason string) { rejected <- reason }
	})

	requestID := managerclient.NewRequestID()
	if err := c.Deactivate(context.Background(), requestID, "Steve"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	select {
	case reason := <-rejected:
		if reason != "既に停止しています" {
			t.Fatalf("OnAdminRejected reason = %q, want 既に停止しています", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminRejected was not called")
	}

	recv := srv.Received()
	if len(recv) != 1 || recv[0].RequestedBy != "Steve" || recv[0].RequestID != requestID {
		t.Fatalf("manager received %+v, want single deactivate{requestedBy:Steve,requestId:%s}", recv, requestID)
	}
}

func TestDeactivateAcceptedTriggersEvacuateHandshake(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "deactivate" {
			return nil
		}
		return []mockmanager.Message{{Type: "evacuate-request", Reason: "deactivate"}}
	})

	evacuated := make(chan string, 1)
	c := newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnEvacuateRequest = func(_ context.Context, reason string) {
			evacuated <- reason
		}
	})

	requestID := managerclient.NewRequestID()
	if err := c.Deactivate(context.Background(), requestID, "Steve"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	select {
	case reason := <-evacuated:
		if reason != "deactivate" {
			t.Fatalf("OnEvacuateRequest reason = %q, want deactivate", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("OnEvacuateRequest was not called")
	}
}

func TestDeactivateCompletePush(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })

	completed := make(chan string, 1)
	newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnAdminCompleted = func(_ context.Context, requestID string) { completed <- requestID }
	})

	srv.Push(mockmanager.Message{Type: "deactivate-complete", RequestID: "req-1"})

	select {
	case got := <-completed:
		if got != "req-1" {
			t.Fatalf("OnAdminCompleted requestID = %q, want req-1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAdminCompleted was not called")
	}
}

func TestSaveData(t *testing.T) {
	// Example lifted verbatim from docs/protocol-gate-manager.md 3.6節.
	events := json.RawMessage(`[
		{"challengeId":"a1b2c3d4-...","type":"death","elapsedTime":900,"timestamp":"2026-07-18T12:05:00Z",
		 "deadPlayer":{"uuid":"11111111-2222-3333-4444-555555555555","name":"Steve"},"killLog":"Steve was slain by Zombie"}
	]`)

	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "savedata-query" {
			return nil
		}
		return []mockmanager.Message{{Type: "savedata-response", Events: events}}
	})

	c := newClient(t, srv.Addr)

	got, err := c.SaveData(context.Background())
	if err != nil {
		t.Fatalf("SaveData: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	e := got[0]
	if e.Type != "death" || e.ElapsedTime != 900 || e.DeadPlayer == nil || e.DeadPlayer.Name != "Steve" || e.KillLog != "Steve was slain by Zombie" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestSenpan(t *testing.T) {
	// Example lifted verbatim from docs/protocol-gate-manager.md 3.7節.
	entries := json.RawMessage(`[
		{"player":{"uuid":"11111111-2222-3333-4444-555555555555","name":"Steve"},"count":3},
		{"player":{"uuid":"22222222-3333-4444-5555-666666666666","name":"Alex"},"count":1}
	]`)

	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "senpan-query" || msg.Mode != "count" {
			return nil
		}
		return []mockmanager.Message{{Type: "senpan-response", Mode: "count", Entries: entries}}
	})

	c := newClient(t, srv.Addr)

	got, err := c.Senpan(context.Background(), "count")
	if err != nil {
		t.Fatalf("Senpan: %v", err)
	}
	if len(got) != 2 || got[0].Player.Name != "Steve" || got[0].Count != 3 || got[1].Player.Name != "Alex" || got[1].Count != 1 {
		t.Fatalf("unexpected entries: %+v", got)
	}
}

func TestHardcoreReadyPush(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })

	ready := make(chan struct{}, 1)
	newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnHardcoreReady = func(context.Context) { ready <- struct{}{} }
	})

	srv.Push(mockmanager.Message{Type: "hardcore-ready"})

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("OnHardcoreReady was not called")
	}
}

// TestConcurrentQueryStates covers the sync lane (QueryState/SaveData/
// Senpan): now that responses are matched by requestID instead of relying
// on only one call ever being in flight, concurrent calls must each get
// their own correct response rather than being serialized or cross-wired.
func TestConcurrentQueryStates(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "state-query" {
			return nil
		}
		return []mockmanager.Message{{Type: "state-response", State: "ready", Running: "true"}}
	})

	c := newClient(t, srv.Addr)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, running, err := c.QueryState(context.Background())
			if err != nil {
				t.Errorf("QueryState: %v", err)
				return
			}
			if state != managerclient.StateReady || running != managerclient.RunningTrue {
				t.Errorf("got state=%q running=%q, want ready/true", state, running)
			}
		}()
	}
	wg.Wait()
}

// TestQueryStateNotBlockedByPendingAdminOp guards against a regression: an
// accepted Start/Load against an already-stopped process can stay pending
// until hardcore fully boots (docs/protocol-gate-manager.md 3.2節), and that
// must never block unrelated synchronous calls like /rta's QueryState,
// which players naturally keep using while waiting for the server to come
// up.
func TestQueryStateNotBlockedByPendingAdminOp(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		switch msg.Type {
		case "state-query":
			return []mockmanager.Message{{Type: "state-response", State: "starting", Running: "true"}}
		default:
			// No reply to "start": simulates hardcore still booting, with
			// no hardcore-ready pushed for the lifetime of this test.
			return nil
		}
	})

	c := newClient(t, srv.Addr)

	requestID := managerclient.NewRequestID()
	if err := c.Start(context.Background(), requestID, false, "Steve"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	qctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, _, err := c.QueryState(qctx)
	if err != nil {
		t.Fatalf("QueryState: %v", err)
	}
	if state != managerclient.StateStarting {
		t.Fatalf("state = %q, want starting", state)
	}
}

// TestOnDisconnectedFiresOnConnectionDrop covers the hook that lets callers
// notice a dropped connection (as opposed to polling Connected()): it must
// fire once the read loop actually exits, not on every reconnect attempt.
func TestOnDisconnectedFiresOnConnectionDrop(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })

	disconnected := make(chan struct{}, 1)
	newClient(t, srv.Addr, func(c *managerclient.Client) {
		c.OnDisconnected = func(context.Context) { disconnected <- struct{}{} }
	})

	srv.CloseConn()

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("OnDisconnected was not called")
	}
}

func TestReconnectAfterDisconnect(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "state-query" {
			return nil
		}
		return []mockmanager.Message{{Type: "state-response", State: "stopped", Running: "false"}}
	})

	c := newClient(t, srv.Addr)

	if _, _, err := c.QueryState(context.Background()); err != nil {
		t.Fatalf("QueryState before disconnect: %v", err)
	}

	srv.CloseConn()

	// Reconnecting can happen faster than this test can observe Connected()
	// dip to false in between, so just assert the client is usable again
	// shortly after the disconnect rather than racing to see the dip.
	waitConnected(t, c)

	// Connected() can flip true while still on the pre-CloseConn connection
	// for a brief moment (the client hasn't noticed the close yet), so a
	// query sent right away can land on a connection that's about to die
	// and never get a reply. Bound each attempt so that case fails fast and
	// gets retried against the connection that replaces it, rather than
	// blocking forever on context.Background().
	deadline := time.Now().Add(2 * time.Second)
	for {
		qctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, _, err := c.QueryState(qctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client never became usable again after reconnect: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueryStateWhenNotConnected(t *testing.T) {
	c := managerclient.New("127.0.0.1:1", logr.Discard()) // Run intentionally not started
	state, running, err := c.QueryState(context.Background())
	if err == nil {
		t.Fatal("expected an error when not connected")
	}
	if state != managerclient.StateUnknown || running != managerclient.RunningUnknown {
		t.Fatalf("got state=%q running=%q, want unknown/unknown", state, running)
	}
}
