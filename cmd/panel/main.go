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

	"github.com/oldbear24/PocketServerManager/internal/audit"
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
	app.RootCmd.AddCommand(auditCommand(p))

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

func auditCommand(p *panel.Panel) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Checks and seals the audit log",
	}

	cmd.AddCommand(&cobra.Command{
		Use:          "verify",
		Short:        "Checks the audit log's chain and seal",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			key := p.AuditKey()
			result, err := audit.Verify(p.App, key)
			if err != nil {
				return err
			}
			if !result.OK {
				return fmt.Errorf("the audit log is broken at entry %d: %s", result.BadSeq, result.Reason)
			}
			fmt.Printf("The audit log is intact: %d entries checked, up to entry %d.\n", result.Checked, result.LastSeq)
			switch {
			case key == nil:
				fmt.Println("Without the master key only the hashes were checked, not the seal.")
			case result.Unsealed > 0:
				fmt.Printf("The newest %d entries were written without the master key; the panel seals them when it starts.\n", result.Unsealed)
			}
			for _, r := range result.Keyless {
				fmt.Printf("Entries %d to %d were written without the master key: only their hashes vouch for them.\n", r[0], r[1])
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "seal",
		Short: "Seals an audit log that nothing proves",
		Long: `Seals an audit log that the panel cannot seal by itself: one that predates the
seal, was only ever written without the master key, or whose seal was removed.
The chain's hashes must match. Every entry the old seal does not prove is
listed as written without the master key, and the seal is recorded as a
security event. Run it only for a log you trust.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			if p.AuditKey() == nil {
				return fmt.Errorf("sealing the audit log needs the master key, which could not be read from %s", p.Config.MasterKeyFile)
			}
			result, err := p.Audit.Seal(audit.System("panel audit seal", audit.SourceCommand))
			if err != nil {
				return err
			}
			if result.To == 0 {
				fmt.Println("The audit log is sealed already.")
				return nil
			}
			fmt.Printf("Sealed the audit log. Entries %d to %d are listed as written without the master key: only their hashes vouch for them.\n", result.From, result.To)
			return nil
		},
	})

	return cmd
}
