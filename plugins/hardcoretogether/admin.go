package hardcoretogether

import (
	"context"
	"sync"

	"go.minekube.com/gate/pkg/command"
)

// adminState tracks, per requestID, the source of an in-flight /start,
// /load or /deactivate plus the message to show once it completes
// successfully. Start/Load/Deactivate return as soon as the request is sent
// (docs/architecture-gate.md 2.2節), so rejection, failure and completion
// all arrive asynchronously; requestID (docs/protocol-gate-manager.md 1節)
// is what lets Gate route each outcome back to the player who actually
// issued that specific request, even if several are in flight from
// different players at once.
type adminState struct {
	mu      sync.Mutex
	pending map[string]adminEntry
}

type adminEntry struct {
	source   command.Source
	doneText string
}

func (s *adminState) set(requestID string, src command.Source, doneText string) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = make(map[string]adminEntry)
	}
	s.pending[requestID] = adminEntry{source: src, doneText: doneText}
	s.mu.Unlock()
}

func (s *adminState) clear(requestID string) {
	s.mu.Lock()
	delete(s.pending, requestID)
	s.mu.Unlock()
}

func (s *adminState) take(requestID string) (command.Source, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.pending[requestID]
	if !ok {
		return nil, "", false
	}
	delete(s.pending, requestID)
	return entry.source, entry.doneText, true
}

// onAdminRejected handles Manager's start-rejected/load-rejected/
// deactivate-rejected (docs/protocol-gate-manager.md 3.4節): show the
// rejection reason to whoever issued the matching request.
func (d *deps) onAdminRejected(_ context.Context, requestID, reason string) {
	src, _, ok := d.admin.take(requestID)
	if !ok {
		return
	}
	_ = src.SendMessage(errorText(reason))
}

// onAdminCompleted handles Manager's hardcore-ready (docs/protocol-gate-manager.md
// 3.1a節) or deactivate-complete (3.5a節): show the matching request's
// completion message to whoever issued it.
func (d *deps) onAdminCompleted(_ context.Context, requestID string) {
	src, text, ok := d.admin.take(requestID)
	if !ok {
		return
	}
	_ = src.SendMessage(infoText(text))
}

// onAdminFailed handles Manager's start-failed/load-failed/
// deactivate-failed (docs/protocol-gate-manager.md 3.5b節): an accepted
// request failed partway through (e.g. the hardcore process crashed before
// becoming ready). If recovered is false, Manager could not confirm the
// process actually stopped, so its state stays stuck "in transition" and
// every admin command keeps getting rejected with "処理中です" until Manager
// itself is restarted (docs/specification.md 2.1節) — surfaced to the
// player so they don't just keep retrying.
func (d *deps) onAdminFailed(_ context.Context, requestID, reason string, recovered bool) {
	src, _, ok := d.admin.take(requestID)
	if !ok {
		return
	}
	text := "サーバーの操作に失敗しました: " + reason
	if !recovered {
		text += "（手動での確認が必要です）"
	}
	_ = src.SendMessage(errorText(text))
}
