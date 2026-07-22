package hardcoretogether

import (
	"context"

	"go.minekube.com/brigodier"
	"go.minekube.com/gate/pkg/command"
)

// loadCommand implements /load <name>, /load <name> force, /load latest and
// /load latest force (docs/specification.md 2.1節). "latest" is just the string
// value of name; resolving it to the newest archive happens on Manager.
// Like /start, this replies immediately with an in-progress notice and the
// eventual rejection or completion arrives later via deps.admin (admin.go).
func loadCommand(d *deps) brigodier.LiteralNodeBuilder {
	run := func(force bool) brigodier.Command {
		return command.Command(func(ctx *command.Context) error {
			name := ctx.String("name")

			reqCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()

			d.admin.set(ctx.Source, "アーカイブの復元が完了しました")
			if err := d.client.Load(reqCtx, name, force, requesterName(ctx.Source)); err != nil {
				d.admin.clear()
				return ctx.Source.SendMessage(errorText("Managerと通信できません: " + err.Error()))
			}
			return ctx.Source.SendMessage(infoText("アーカイブを復元しています..."))
		})
	}

	return brigodier.Literal("load").
		Requires(requiresPermission(AdminPermission)).
		Then(brigodier.Argument("name", brigodier.String).
			Executes(run(false)).
			Then(brigodier.Literal("force").Executes(run(true))))
}
