package hardcoretogether

import (
	"context"
	"time"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"
)

// startupTimeout bounds /start and /load's wait for Manager's terminal
// response. When the target process was already stopped, Start/Load are
// accepted with no immediate acknowledgement (docs/protocol-gate-manager.md
// 3.2節) and only resolve once hardcore actually finishes booting
// (hardcore-ready) — a full server startup can run well past commandTimeout.
// The exact bound is one of protocol-gate-manager.md 5節's open items; this
// is a conservative placeholder.
const startupTimeout = 5 * time.Minute

// startCommand implements /start and /start clean (docs/specification.md 2.1節).
// The process/world state checks themselves happen on Manager; this only
// forwards the request and reports the outcome.
func startCommand(d *deps) brigodier.LiteralNodeBuilder {
	run := func(clean bool) brigodier.Command {
		return command.Command(func(ctx *command.Context) error {
			reqCtx, cancel := context.WithTimeout(context.Background(), startupTimeout)
			defer cancel()

			result, err := d.client.Start(reqCtx, clean, requesterName(ctx.Source))
			if err != nil {
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
			}
			if result.Rejected {
				return ctx.Source.SendMessage(errorText(result.Reason))
			}
			if clean {
				return ctx.Source.SendMessage(infoText("挑戦をリセットしています..."))
			}
			return ctx.Source.SendMessage(infoText("起動しています..."))
		})
	}

	return brigodier.Literal("start").
		Requires(requiresPermission(AdminPermission)).
		Executes(run(false)).
		Then(brigodier.Literal("clean").Executes(run(true)))
}
