package hardcoretogether

import (
	"context"
	"sync"

	"go.minekube.com/gate/pkg/command"
)

// adminState tracks the source of the currently in-flight /start, /load or
// /deactivate, if any, plus the message to show once it completes
// successfully. Start/Load/Deactivate return as soon as the request is
// sent (docs/architecture-gate.md 2.2節), so both the eventual rejection
// and the eventual completion arrive asynchronously with no correlation ID
// (docs/protocol-gate-manager.md); Gate never has two such commands racing
// against Manager at once in this implementation, so a single pending slot
// is enough.
type adminState struct {
	mu       sync.Mutex
	pending  command.Source
	doneText string
}

func (s *adminState) set(src command.Source, doneText string) {
	s.mu.Lock()
	s.pending, s.doneText = src, doneText
	s.mu.Unlock()
}

func (s *adminState) clear() {
	s.mu.Lock()
	s.pending, s.doneText = nil, ""
	s.mu.Unlock()
}

func (s *adminState) take() (command.Source, string) {
	s.mu.Lock()
	src, text := s.pending, s.doneText
	s.pending, s.doneText = nil, ""
	s.mu.Unlock()
	return src, text
}

// onAdminRejected handles Manager's start-rejected/load-rejected/
// deactivate-rejected (docs/protocol-gate-manager.md 3.4節): show the
// rejection reason to whoever issued the pending command.
func (d *deps) onAdminRejected(_ context.Context, reason string) {
	src, _ := d.admin.take()
	if src == nil {
		return
	}
	_ = src.SendMessage(errorText(reason))
}

// onAdminCompleted handles Manager's hardcore-ready (docs/protocol-gate-manager.md
// 3.1a節) or deactivate-complete (3.5a節): show the pending command's
// completion message to whoever issued it.
func (d *deps) onAdminCompleted(_ context.Context) {
	src, text := d.admin.take()
	if src == nil {
		return
	}
	_ = src.SendMessage(infoText(text))
}
