// Command panel is the PocketCloud panel: PocketBase embedded as a library, the
// React UI and the gRPC gateway for agents, in one binary.
package main

import (
	"fmt"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/spf13/cobra"

	_ "github.com/oldbear24/PocketServerManager/migrations"

	"github.com/oldbear24/PocketServerManager/internal/panel"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/version"
	"github.com/oldbear24/PocketServerManager/web"
)

func main() {
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: os.Getenv(panel.EnvDataDir),
	})
	app.RootCmd.Use = "panel"
	app.RootCmd.Short = "PocketCloud panel"
	app.RootCmd.Version = version.Version

	config := panel.ConfigFromEnv()
	config.BindFlags(app.RootCmd)

	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})

	p := panel.New(app, &config, web.FS())
	p.EnableDiagnostics()
	p.Register()

	app.RootCmd.AddCommand(keygenCommand())

	if err := app.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func keygenCommand() *cobra.Command {
	var out string

	cmd := &cobra.Command{
		Use:          "keygen",
		Short:        "Creates a new master key file",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			if err := secrets.WriteKeyFile(out); err != nil {
				return err
			}
			fmt.Printf("Master key written to %s.\nKeep a copy outside this machine: without it the stored secrets cannot be read.\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "master.key", "where to write the key")

	return cmd
}
