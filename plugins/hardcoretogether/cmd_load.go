package hardcoretogether

import (
	"context"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"

	"github.com/minekube/gate-plugin-template/plugins/hardcoretogether/managerclient"
)

// loadCommand implements /load <name>, /load <name> force, /load latest and
// /load latest force (docs/specification.md 2.1節). "latest" is just the string
// value of name; resolving it to the newest archive happens on Manager. The
// eventual rejection, failure or completion arrives later via deps.admin
// (admin.go), correlated by requestID.
func loadCommand(d *deps) brigodier.LiteralNodeBuilder {
	run := func(force bool) brigodier.Command {
		return command.Command(func(ctx *command.Context) error {
			name := ctx.String("name")

			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			requestID := managerclient.NewRequestID()
			d.admin.set(requestID, ctx.Source, "アーカイブの復元が完了しました")

			// Sent before contacting Manager — see cmd_start.go's comment
			// on why the ordering matters (docs/architecture-gate.md 2.2節).
			d.notify(ctx.Source, infoText("アーカイブを復元しています..."))

			if err := d.client.Load(reqCtx, requestID, name, force, requesterName(ctx.Source)); err != nil {
				d.admin.clear(requestID)
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
			}
			return nil
		})
	}

	return brigodier.Literal("load").
		Requires(requiresPermission(AdminPermission)).
		Then(brigodier.Argument("name", brigodier.String).
			Executes(run(false)).
			Then(brigodier.Literal("force").Executes(run(true))))
}
