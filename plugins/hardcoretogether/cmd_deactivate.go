package hardcoretogether

import (
	"context"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"

	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/managerclient"
)

// deactivateCommand implements /deactivate (docs/specification.md 2.1節): stops
// the hardcore process without touching world contents or the running
// value. The eventual rejection, failure or "stopped" completion arrives
// later via deps.admin (admin.go), correlated by requestID.
func deactivateCommand(d *deps) brigodier.LiteralNodeBuilder {
	return brigodier.Literal("deactivate").
		Requires(requiresPermission(AdminPermission)).
		Executes(command.Command(func(ctx *command.Context) error {
			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			requestID := managerclient.NewRequestID()
			d.admin.set(requestID, ctx.Source, "サーバーを停止しました")

			// Sent before contacting Manager — see cmd_start.go's comment
			// on why the ordering matters (docs/architecture-gate.md 2.2節).
			d.notify(ctx.Source, infoText("サーバーを停止しています..."))

			if err := d.client.Deactivate(reqCtx, requestID, requesterName(ctx.Source)); err != nil {
				d.admin.clear(requestID)
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
			}
			return nil
		}))
}
