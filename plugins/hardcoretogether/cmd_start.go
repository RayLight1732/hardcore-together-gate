package hardcoretogether

import (
	"context"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"
)

// startCommand implements /start and /start clean (docs/specification.md 2.1節).
// The process/world state checks themselves happen on Manager; this only
// forwards the request and reports the outcome. Manager gives no
// synchronous accept signal (docs/protocol-gate-manager.md 3.2節), so the
// command replies immediately with an in-progress notice and the eventual
// rejection or completion arrives later via deps.admin (admin.go).
func startCommand(d *deps) brigodier.LiteralNodeBuilder {
	run := func(clean bool) brigodier.Command {
		return command.Command(func(ctx *command.Context) error {
			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			d.admin.set(ctx.Source, "起動が完了しました")
			if err := d.client.Start(reqCtx, clean, requesterName(ctx.Source)); err != nil {
				d.admin.clear()
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
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
