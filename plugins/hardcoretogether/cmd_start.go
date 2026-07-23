package hardcoretogether

import (
	"context"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"

	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/managerclient"
)

// startCommand implements /start and /start clean (docs/specification.md 2.1節).
// The process/world state checks themselves happen on Manager; this only
// forwards the request and reports the outcome. Manager gives no
// synchronous accept signal (docs/protocol-gate-manager.md 3.2節), so the
// command replies immediately with an in-progress notice and the eventual
// rejection, failure or completion arrives later via deps.admin (admin.go),
// correlated by requestID.
func startCommand(d *deps) brigodier.LiteralNodeBuilder {
	run := func(clean bool) brigodier.Command {
		return command.Command(func(ctx *command.Context) error {
			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			requestID := managerclient.NewRequestID()
			d.admin.set(requestID, ctx.Source, "起動が完了しました")
			if err := d.client.Start(reqCtx, requestID, clean, requesterName(ctx.Source)); err != nil {
				d.admin.clear(requestID)
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
