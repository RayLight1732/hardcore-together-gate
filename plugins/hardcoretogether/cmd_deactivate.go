package hardcoretogether

import (
	"context"
	"sync"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"
)

// deactivateState tracks the source of the currently in-flight /deactivate,
// if any, so the eventual (asynchronous) deactivate-complete notification
// can be shown to whoever ran the command (docs/protocol-gate-manager.md
// 3.5a節). The protocol carries no correlation ID and Gate never has two
// such commands racing against Manager at once in this implementation
// (docs/architecture-gate.md 2.2節), so a single pending slot is enough.
type deactivateState struct {
	mu      sync.Mutex
	pending command.Source
}

func (s *deactivateState) set(src command.Source) {
	s.mu.Lock()
	s.pending = src
	s.mu.Unlock()
}

func (s *deactivateState) take() command.Source {
	s.mu.Lock()
	src := s.pending
	s.pending = nil
	s.mu.Unlock()
	return src
}

// deactivateCommand implements /deactivate (docs/specification.md 2.1節): stops
// the hardcore process without touching world contents or the running
// value. It is only accepted while the process is running, so an accepted
// request always evacuates first (docs/protocol-gate-manager.md 3.5節)
// before Manager confirms with deactivate-complete.
func deactivateCommand(d *deps) brigodier.LiteralNodeBuilder {
	return brigodier.Literal("deactivate").
		Requires(requiresPermission(AdminPermission)).
		Executes(command.Command(func(ctx *command.Context) error {
			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			result, err := d.client.Deactivate(reqCtx, requesterName(ctx.Source))
			if err != nil {
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
			}
			if result.Rejected {
				return ctx.Source.SendMessage(errorText(result.Reason))
			}
			d.deactivate.set(ctx.Source)
			return ctx.Source.SendMessage(infoText("サーバーを停止しています..."))
		}))
}

// onDeactivateComplete handles Manager's deactivate-complete
// (docs/protocol-gate-manager.md 3.5a節): tell whoever ran /deactivate that
// the process actually stopped.
func (d *deps) onDeactivateComplete(_ context.Context) {
	src := d.deactivate.take()
	if src == nil {
		return
	}
	_ = src.SendMessage(infoText("サーバーを停止しました"))
}
