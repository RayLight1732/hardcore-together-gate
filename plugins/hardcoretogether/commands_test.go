package hardcoretogether

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	c "go.minekube.com/common/minecraft/component"
	"go.minekube.com/gate/pkg/command"
	"go.minekube.com/gate/pkg/util/permission"

	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/internal/mockmanager"
	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/managerclient"
)

// fakeSource is a minimal command.Source for driving commands in tests
// without a real Minecraft client connection.
type fakeSource struct {
	allowed bool

	mu       sync.Mutex
	messages []string
}

func (f *fakeSource) HasPermission(string) bool { return f.allowed }

func (f *fakeSource) PermissionValue(string) permission.TriState {
	if f.allowed {
		return permission.True
	}
	return permission.Undefined
}

func (f *fakeSource) SendMessage(msg c.Component, _ ...command.MessageOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if text, ok := msg.(*c.Text); ok {
		f.messages = append(f.messages, text.Content)
	}
	return nil
}

func (f *fakeSource) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return ""
	}
	return f.messages[len(f.messages)-1]
}

// waitForMessage polls src.last() until it contains want, since Start/Load/
// Deactivate now reply immediately and their eventual rejection/completion
// arrives asynchronously via deps.admin (admin.go).
func waitForMessage(t *testing.T, src *fakeSource, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if strings.Contains(src.last(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("last message = %q, want it to contain %q", src.last(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// firstReceivedRequestID waits for srv to have received at least one message
// and returns its requestID. The mock server appends to its received list
// from its own goroutine reading the socket, so this can lag slightly
// behind the send() call on Gate's side returning.
func firstReceivedRequestID(t *testing.T, srv *mockmanager.Server) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if recv := srv.Received(); len(recv) > 0 {
			return recv[0].RequestID
		}
		if time.Now().After(deadline) {
			t.Fatal("manager never received a message")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newTestDeps connects a managerclient.Client to srv and wraps it in deps
// with no *proxy.Proxy. This is enough to exercise /start, /load,
// /deactivate, /savedata and /senpan, which never touch d.proxy directly;
// /rta, /lobby and the evacuate/transfer callbacks need a real player
// connection and are out of scope for this test tier.
func newTestDeps(t *testing.T, srv *mockmanager.Server) *deps {
	t.Helper()
	client := managerclient.New(srv.Addr, logr.Discard())
	d := &deps{client: client, log: logr.Discard()}
	// onAdminRejected/onAdminCompleted/onAdminFailed/onDisconnected never
	// touch d.proxy (unlike OnEvacuateRequest/OnHardcoreReady), so it's
	// safe to wire them up at this test tier.
	client.OnAdminRejected = d.onAdminRejected
	client.OnAdminCompleted = d.onAdminCompleted
	client.OnAdminFailed = d.onAdminFailed
	client.OnDisconnected = d.onDisconnected

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go client.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for !client.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("client never connected to mock manager")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return d
}

func newTestManager(d *deps) *command.Manager {
	mgr := &command.Manager{}
	mgr.Register(startCommand(d))
	mgr.Register(loadCommand(d))
	mgr.Register(deactivateCommand(d))
	mgr.Register(savedataCommand(d))
	mgr.Register(senpanCommand(d))
	return mgr
}

// TestStartCommand_Rejected covers rejection: /start replies immediately
// with an in-progress notice (it never waits for Manager), so the rejection
// reason only reaches src asynchronously via deps.admin once start-rejected
// arrives.
func TestStartCommand_Rejected(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		return []mockmanager.Message{{Type: "start-rejected", Reason: "挑戦が進行中です"}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "start"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	waitForMessage(t, src, "挑戦が進行中です")
}

// TestStartCommand_CleanAccepted covers /start clean: the command replies
// immediately, evacuation (if any) happens independently, and the eventual
// hardcore-ready delivers the completion notice via deps.admin.
func TestStartCommand_CleanAccepted(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		if !msg.Clean {
			t.Errorf("start message had clean=false, want true for /start clean")
		}
		return []mockmanager.Message{{Type: "evacuate-request", Reason: "force-reset"}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "start clean"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "リセット") {
		t.Fatalf("last message = %q, want an in-progress notice", src.last())
	}

	srv.Push(mockmanager.Message{Type: "hardcore-ready", RequestID: firstReceivedRequestID(t, srv)})
	waitForMessage(t, src, "完了")
}

// TestStartCommand_PlainAccepted covers /start (no clean) against an
// already-stopped process: Manager accepts with no immediate
// acknowledgement at all (docs/protocol-gate-manager.md 3.2節), but the
// command still replies immediately; the completion notice only arrives
// once hardcore-ready is pushed.
func TestStartCommand_PlainAccepted(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		if msg.Clean {
			t.Errorf("start message had clean=true, want false for plain /start")
		}
		return nil
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "start"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "起動して") {
		t.Fatalf("last message = %q, want an in-progress notice", src.last())
	}

	srv.Push(mockmanager.Message{Type: "hardcore-ready", RequestID: firstReceivedRequestID(t, srv)})
	waitForMessage(t, src, "完了")
}

// TestStartCommand_Failed covers start-failed (docs/protocol-gate-manager.md
// 3.5b節): an accepted request that fails after acceptance (e.g. the
// process crashed before becoming ready) surfaces reason and, when
// recovered is false, an extra note that retrying alone may not help.
func TestStartCommand_Failed(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		return nil
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "start"); err != nil {
		t.Fatalf("Do: %v", err)
	}

	srv.Push(mockmanager.Message{
		Type:      "start-failed",
		RequestID: firstReceivedRequestID(t, srv),
		Reason:    "process exited before ready",
		Recovered: false,
	})
	waitForMessage(t, src, "process exited before ready")
	if !strings.Contains(src.last(), "手動") {
		t.Fatalf("last message = %q, want it to mention manual confirmation since recovered=false", src.last())
	}
}

// TestStartCommand_NotifiedOnDisconnect guards against a pending admin
// request hanging forever: if Manager accepts a /start and then the
// connection drops before any rejection/failure/completion arrives, the
// player must be told the outcome is unknown rather than being left with
// only their original "起動しています" notice and no further word, ever.
func TestStartCommand_NotifiedOnDisconnect(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "start" {
			return nil
		}
		return nil // accepted silently; never resolves before the connection drops
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "start"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "起動して") {
		t.Fatalf("last message = %q, want an in-progress notice", src.last())
	}
	firstReceivedRequestID(t, srv) // wait for Manager to actually receive it

	srv.CloseConn()

	waitForMessage(t, src, "接続が切れた")
}

// TestConcurrentStartCommands_RoutedIndependently is the end-to-end
// regression test for the bug this requestID mechanism fixes: two players
// running /start around the same time used to share a single pending
// slot, so the second player's request would silently overwrite the
// first's — meaning the first player would see their initial "起動しています"
// notice and then nothing else, ever. Each player must now get their own
// correct outcome regardless of overlap.
func TestConcurrentStartCommands_RoutedIndependently(t *testing.T) {
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
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)

	alice := &fakeSource{allowed: true}
	if err := mgr.Do(context.Background(), alice, "start"); err != nil {
		t.Fatalf("Do (alice): %v", err)
	}
	aliceRequestID := firstReceivedRequestID(t, srv)

	bob := &fakeSource{allowed: true}
	if err := mgr.Do(context.Background(), bob, "start"); err != nil {
		t.Fatalf("Do (bob): %v", err)
	}

	// Bob's request loses the race and must see his own rejection, not
	// Alice's outcome.
	waitForMessage(t, bob, "処理中です")

	// Completing Alice's request must reach Alice, not Bob.
	srv.Push(mockmanager.Message{Type: "hardcore-ready", RequestID: aliceRequestID})
	waitForMessage(t, alice, "完了")

	if strings.Contains(bob.last(), "完了") {
		t.Fatalf("bob's last message = %q, should not have received Alice's completion notice", bob.last())
	}
}

func TestStartCommand_RequiresPermission(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: false}

	if err := mgr.Do(context.Background(), src, "start"); err == nil {
		t.Fatal("expected an error for an unprivileged source, got none")
	}
	if recv := srv.Received(); len(recv) != 0 {
		t.Fatalf("manager received %+v, want nothing (command should have been blocked)", recv)
	}
}

func TestLoadCommand(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "load" {
			return nil
		}
		if msg.Name != "latest" {
			t.Errorf("load message name = %q, want latest", msg.Name)
		}
		return []mockmanager.Message{{Type: "load-rejected", Reason: "アーカイブ latest は存在しません"}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "load latest"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	waitForMessage(t, src, "アーカイブ latest は存在しません")
}

func TestDeactivateCommand_Rejected(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "deactivate" {
			return nil
		}
		return []mockmanager.Message{{Type: "deactivate-rejected", Reason: "既に停止しています"}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "deactivate"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	waitForMessage(t, src, "既に停止しています")
}

func TestDeactivateCommand_RequiresPermission(t *testing.T) {
	srv := mockmanager.Start(t, func(mockmanager.Message) []mockmanager.Message { return nil })
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: false}

	if err := mgr.Do(context.Background(), src, "deactivate"); err == nil {
		t.Fatal("expected an error for an unprivileged source, got none")
	}
	if recv := srv.Received(); len(recv) != 0 {
		t.Fatalf("manager received %+v, want nothing (command should have been blocked)", recv)
	}
}

// TestDeactivateCommand_CompletesAfterEvacuation covers the full accept →
// evacuate → deactivate-complete flow. onAdminCompleted never touches
// d.proxy, so unlike evacuate.go/transfer.go's callbacks this is testable
// at this tier (docs/architecture-gate.md 7.3節).
func TestDeactivateCommand_CompletesAfterEvacuation(t *testing.T) {
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "deactivate" {
			return nil
		}
		return []mockmanager.Message{{Type: "evacuate-request", Reason: "deactivate"}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "deactivate"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "停止しています") {
		t.Fatalf("last message = %q, want an in-progress notice", src.last())
	}

	srv.Push(mockmanager.Message{Type: "deactivate-complete", RequestID: firstReceivedRequestID(t, srv)})
	waitForMessage(t, src, "停止しました")
}

func TestSavedataCommand(t *testing.T) {
	events := json.RawMessage(`[
		{"challengeId":"a1b2c3d4-...","type":"clear","elapsedTime":1500,"timestamp":"2026-07-18T12:45:00Z",
		 "trigger":{"kind":"boss","mobId":"twilightforest:ur_ghast"}}
	]`)
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "savedata-query" {
			return nil
		}
		return []mockmanager.Message{{Type: "savedata-response", Events: events}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "savedata"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "a1b2c3d4-...") || !strings.Contains(src.last(), "クリア") {
		t.Fatalf("last message = %q, want it to mention the challengeId and clear event", src.last())
	}
}

func TestSenpanCommand(t *testing.T) {
	entries := json.RawMessage(`[{"player":{"uuid":"11111111-2222-3333-4444-555555555555","name":"Steve"},"count":3}]`)
	srv := mockmanager.Start(t, func(msg mockmanager.Message) []mockmanager.Message {
		if msg.Type != "senpan-query" || msg.Mode != "list" {
			return nil
		}
		return []mockmanager.Message{{Type: "senpan-response", Mode: "list", Entries: entries}}
	})
	d := newTestDeps(t, srv)
	mgr := newTestManager(d)
	src := &fakeSource{allowed: true}

	if err := mgr.Do(context.Background(), src, "senpan list"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(src.last(), "Steve") || !strings.Contains(src.last(), "3回") {
		t.Fatalf("last message = %q, want it to mention Steve and 3回", src.last())
	}
}
