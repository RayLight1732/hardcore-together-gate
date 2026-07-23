// Package managerclient implements the Gate side of the Gate⇔Manager
// signal protocol documented in docs/protocol-gate-manager.md.
package managerclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
)

// ErrNotConnected is returned by calls made while there is no active
// connection to Manager.
var ErrNotConnected = errors.New("managerclient: not connected to manager")

// NewRequestID returns a new unique ID for correlating a request with its
// eventual response (docs/protocol-gate-manager.md 1節). Callers that need
// to track a pending request themselves (Start/Load/Deactivate) must
// generate one before sending, since those calls return immediately and
// never wait for a reply here.
func NewRequestID() string {
	return uuid.NewString()
}

// State is hardcore's lifecycle state as tracked by Manager.
type State string

const (
	// StateUnknown is a Gate-local value meaning the state could not be
	// determined (not connected to Manager, or the query failed). It is
	// never sent by Manager itself.
	StateUnknown  State = "unknown"
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopping State = "stopping"
)

// Running is hardcore's running flag as cached by Manager.
type Running string

const (
	RunningTrue    Running = "true"
	RunningFalse   Running = "false"
	RunningUnknown Running = "unknown"
)

// RecordEvent is one entry of a /savedata response (docs/protocol-gate-manager.md 3.6節).
type RecordEvent struct {
	ChallengeID string     `json:"challengeId"`
	Type        string     `json:"type"` // save | death | clear
	ElapsedTime int64      `json:"elapsedTime"`
	Timestamp   string     `json:"timestamp"`
	ArchiveName string     `json:"archiveName,omitempty"`
	Trigger     *Trigger   `json:"trigger,omitempty"`
	DeadPlayer  *PlayerRef `json:"deadPlayer,omitempty"`
	KillLog     string     `json:"killLog,omitempty"`
}

// Trigger identifies what caused a save/clear event.
type Trigger struct {
	Kind   string `json:"kind"` // boss | manual
	MobID  string `json:"mobId,omitempty"`
	Player string `json:"player,omitempty"`
}

// PlayerRef identifies a player by UUID and name.
type PlayerRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// SenpanEntry is one entry of a /senpan response (docs/protocol-gate-manager.md 3.7節).
type SenpanEntry struct {
	Player PlayerRef `json:"player"`
	Count  int       `json:"count"`
}

// message is the wire representation of every Gate⇔Manager NDJSON message.
// A single loosely-typed struct is used because the protocol is small
// (docs/protocol-gate-manager.md, ~12 message types) and every field maps
// 1:1 to a documented field of some message.
type message struct {
	Type string `json:"type"`

	// Every message (docs/protocol-gate-manager.md 1節). Gate generates
	// this for every request it sends; Manager echoes it back on the
	// corresponding response so responses can be matched to the request
	// that caused them regardless of how many are in flight at once.
	RequestID string `json:"requestId,omitempty"`

	// state-response
	State   string `json:"state,omitempty"`
	Running string `json:"running,omitempty"`

	// start
	Clean bool `json:"clean,omitempty"`

	// load
	Force bool `json:"force,omitempty"`

	// start / load / deactivate
	RequestedBy string `json:"requestedBy,omitempty"`
	Name        string `json:"name,omitempty"`

	// start-rejected / load-rejected / deactivate-rejected / evacuate-request /
	// start-failed / load-failed / deactivate-failed
	Reason string `json:"reason,omitempty"`

	// start-failed / load-failed / deactivate-failed
	Recovered bool `json:"recovered,omitempty"`

	// savedata-response
	Events []RecordEvent `json:"events,omitempty"`

	// senpan-query / senpan-response
	Mode    string        `json:"mode,omitempty"`
	Entries []SenpanEntry `json:"entries,omitempty"`
}

// Client is a persistent connection to Manager. Create with New, set any of
// the On* callback fields below, then run Client.Run in a goroutine for the
// lifetime of the plugin — in that order. The On* fields are plain struct
// fields with no internal synchronization, so they must all be assigned
// before the first call to Run and never reassigned afterward; Run's
// internal goroutine reads them concurrently once started, and a later
// write from another goroutine would be a data race.
type Client struct {
	addr string
	log  logr.Logger

	// OnEvacuateRequest is called synchronously for every evacuate-request
	// received (docs/protocol-gate-manager.md 3.5節). The client sends
	// evacuate-complete automatically once it returns.
	OnEvacuateRequest func(ctx context.Context, reason string)
	// OnHardcoreReady is called for every hardcore-ready notification
	// (docs/protocol-gate-manager.md 3.1a節), regardless of which Start/Load
	// request caused it — this drives the lobby-wide auto-transfer, which
	// applies to every player in lobby, not just whoever ran the command.
	OnHardcoreReady func(ctx context.Context)
	// OnAdminRejected is called whenever Manager rejects a Start/Load/
	// Deactivate request (start-rejected/load-rejected/deactivate-rejected,
	// docs/protocol-gate-manager.md 3.4節). Those calls return as soon as
	// the request is sent, so this is the only way rejection reaches Gate.
	// requestID identifies which request this rejection is for.
	OnAdminRejected func(ctx context.Context, requestID, reason string)
	// OnAdminCompleted is called once an accepted Start/Load/Deactivate
	// actually finishes: hardcore-ready for Start/Load
	// (docs/protocol-gate-manager.md 3.1a節), deactivate-complete for
	// Deactivate (3.5a節). requestID identifies which request completed.
	OnAdminCompleted func(ctx context.Context, requestID string)
	// OnAdminFailed is called when an accepted Start/Load/Deactivate fails
	// after acceptance (start-failed/load-failed/deactivate-failed,
	// docs/protocol-gate-manager.md 3.5b節) — e.g. the hardcore process
	// crashed before becoming ready. recovered reports whether Manager
	// confirmed the process is actually not running and reset its state
	// accordingly; if false, Manager's process state stays "in transition"
	// and every admin command keeps getting rejected with "処理中です" until
	// Manager itself is restarted (docs/specification.md 2.1節).
	OnAdminFailed func(ctx context.Context, requestID, reason string, recovered bool)
	// OnDisconnected is called once whenever the TCP connection to Manager
	// drops (after having been up), before Run starts retrying. Any
	// request accepted before the drop can no longer be corresponded to a
	// response — reconnecting opens a fresh TCP stream that Manager may
	// reply on with no memory of what was in flight on the old one — so
	// callers should treat every request still pending at this point as
	// permanently unresolved and notify accordingly, rather than leaving
	// it to hang forever.
	OnDisconnected func(ctx context.Context)

	connMu sync.Mutex
	conn   net.Conn

	writeMu sync.Mutex

	// reqMu/waiters back the synchronous request/response calls
	// (QueryState/SaveData/Senpan): call() registers a channel under its
	// own requestID and deliver() routes each response to the matching
	// entry, so concurrent calls no longer need to be serialized against
	// each other (docs/protocol-gate-manager.md 1節's requestId replaces
	// the previous "only one call in flight" assumption). Start/Load/
	// Deactivate never register here — they don't wait for a reply at all
	// (see their doc comments) — so this map only ever holds sync-lane
	// waiters.
	reqMu   sync.Mutex
	waiters map[string]chan message
}

// New creates a Client that will connect to addr once Run is started.
func New(addr string, log logr.Logger) *Client {
	return &Client{addr: addr, log: log, waiters: make(map[string]chan message)}
}

// Connected reports whether the TCP connection to Manager is currently up.
func (c *Client) Connected() bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn != nil
}

// Run connects to Manager and reconnects with backoff until ctx is done.
// It blocks; callers should run it in a goroutine.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.addr)
		if err != nil {
			c.log.Error(err, "failed to connect to manager, retrying", "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second
		c.log.Info("connected to manager")
		c.setConn(conn)

		c.readLoop(conn)

		c.setConn(nil)
		_ = conn.Close()
		c.log.Info("disconnected from manager, will reconnect")
		if c.OnDisconnected != nil {
			go c.OnDisconnected(context.Background())
		}
	}
}

func (c *Client) setConn(conn net.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *Client) readLoop(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			c.log.Error(err, "failed to parse message from manager", "line", string(line))
			continue
		}
		c.dispatch(msg)
	}
}

// dispatch routes an incoming message either to a pending call() on the
// synchronous lane (state-query/savedata-query/senpan-query — the only
// messages that ever go through call()) or to one of the callbacks used by
// everything else: evacuate-request/hardcore-ready (unprompted
// notifications) and start-rejected/load-rejected/deactivate-rejected/
// start-failed/load-failed/deactivate-failed/deactivate-complete
// (asynchronous outcomes of Start/Load/Deactivate, which never wait on the
// sync lane — see their doc comments).
func (c *Client) dispatch(msg message) {
	switch msg.Type {
	case "hardcore-ready":
		if c.OnAdminCompleted != nil {
			go c.OnAdminCompleted(context.Background(), msg.RequestID)
		}
		if c.OnHardcoreReady != nil {
			go c.OnHardcoreReady(context.Background())
		}
	case "deactivate-complete":
		if c.OnAdminCompleted != nil {
			go c.OnAdminCompleted(context.Background(), msg.RequestID)
		}
	case "evacuate-request":
		go c.handleEvacuateRequest(msg)
	case "start-rejected", "load-rejected", "deactivate-rejected":
		if c.OnAdminRejected != nil {
			go c.OnAdminRejected(context.Background(), msg.RequestID, msg.Reason)
		}
	case "start-failed", "load-failed", "deactivate-failed":
		if c.OnAdminFailed != nil {
			go c.OnAdminFailed(context.Background(), msg.RequestID, msg.Reason, msg.Recovered)
		}
	default:
		c.deliver(msg)
	}
}

func (c *Client) deliver(msg message) {
	c.reqMu.Lock()
	ch, ok := c.waiters[msg.RequestID]
	c.reqMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

func (c *Client) handleEvacuateRequest(msg message) {
	if c.OnEvacuateRequest != nil {
		c.OnEvacuateRequest(context.Background(), msg.Reason)
	}
	if err := c.send(message{Type: "evacuate-complete", RequestID: msg.RequestID}); err != nil {
		c.log.Error(err, "failed to send evacuate-complete")
	}
}

func (c *Client) send(msg message) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = conn.Write(data)
	return err
}

// call sends req (assigning it a fresh requestID) and waits for the
// response Manager sends back carrying that same requestID. Multiple calls
// may be in flight concurrently — each is tracked under its own requestID
// in waiters, so responses are routed correctly regardless of arrival order
// (docs/protocol-gate-manager.md 1節).
func (c *Client) call(ctx context.Context, req message) (message, error) {
	if !c.Connected() {
		return message{}, ErrNotConnected
	}

	req.RequestID = NewRequestID()

	ch := make(chan message, 1)
	c.reqMu.Lock()
	c.waiters[req.RequestID] = ch
	c.reqMu.Unlock()
	defer func() {
		c.reqMu.Lock()
		delete(c.waiters, req.RequestID)
		c.reqMu.Unlock()
	}()

	if err := c.send(req); err != nil {
		return message{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return message{}, ctx.Err()
	}
}

// QueryState asks Manager for hardcore's current state (docs/protocol-gate-manager.md 3.1節).
func (c *Client) QueryState(ctx context.Context) (State, Running, error) {
	resp, err := c.call(ctx, message{Type: "state-query"})
	if err != nil {
		return StateUnknown, RunningUnknown, err
	}
	return State(resp.State), Running(resp.Running), nil
}

// Start sends a /start [clean] request (docs/protocol-gate-manager.md 3.2節).
// requestID must be generated by the caller (NewRequestID) before calling,
// and used to track the pending request, since Start returns as soon as the
// request is sent — Manager gives no synchronous accept/reject reply for
// start/load/deactivate (only a rejection is ever synchronous, and even
// that isn't waited for here), so the outcome (rejection via
// OnAdminRejected, failure via OnAdminFailed, or eventual completion via
// OnAdminCompleted once hardcore-ready arrives) is always delivered
// asynchronously, identified by requestID. See docs/architecture-gate.md 2.2節.
func (c *Client) Start(ctx context.Context, requestID string, clean bool, requestedBy string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.send(message{Type: "start", RequestID: requestID, Clean: clean, RequestedBy: requestedBy})
}

// Load sends a /load request (docs/protocol-gate-manager.md 3.3節). name may
// be "latest". Like Start, it only sends the request (requestID must be
// generated by the caller); the outcome arrives later via
// OnAdminRejected/OnAdminFailed/OnAdminCompleted, identified by requestID.
func (c *Client) Load(ctx context.Context, requestID string, name string, force bool, requestedBy string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.send(message{Type: "load", RequestID: requestID, Name: name, Force: force, RequestedBy: requestedBy})
}

// Deactivate sends a /deactivate request (docs/protocol-gate-manager.md
// 3.3a節). Like Start/Load, it only sends the request (requestID must be
// generated by the caller); rejection, failure, or the eventual
// deactivate-complete arrives later via OnAdminRejected/OnAdminFailed/
// OnAdminCompleted, identified by requestID.
func (c *Client) Deactivate(ctx context.Context, requestID string, requestedBy string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.send(message{Type: "deactivate", RequestID: requestID, RequestedBy: requestedBy})
}

// SaveData requests the /savedata listing (docs/protocol-gate-manager.md 3.6節).
func (c *Client) SaveData(ctx context.Context) ([]RecordEvent, error) {
	resp, err := c.call(ctx, message{Type: "savedata-query"})
	if err != nil {
		return nil, err
	}
	return resp.Events, nil
}

// Senpan requests the /senpan aggregation (docs/protocol-gate-manager.md 3.7節). mode is "list" or "count".
func (c *Client) Senpan(ctx context.Context, mode string) ([]SenpanEntry, error) {
	resp, err := c.call(ctx, message{Type: "senpan-query", Mode: mode})
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}
