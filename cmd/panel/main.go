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
	// before PocketBase: each of its commands bootstraps pb_data, which
	// would make a database where the backup is to go (#32)
	if len(os.Args) > 1 && os.Args[1] == "recover" {
		os.Exit(runRecover(os.Args[2:]))
	}

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: os.Getenv(panel.EnvDataDir),
	})
	app.RootCmd.Use = "panel"
	app.RootCmd.Short = "PocketCloud panel"
	app.RootCmd.Version = version.Version
	app.RootCmd.Long = "PocketCloud panel.\n\nA panel lost with its machine comes back from its backup with `panel recover`, run as\n" +
		"the first argument, before PocketBase opens pb_data: see `panel recover --help` and\ndocs/disaster-recovery.md."

	config := panel.ConfigFromEnv()
	config.BindFlags(app.RootCmd)
	// one HTTPS at a time (#63)
	app.RootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error { return config.CheckServe(cmd, args) }

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

// runRecover runs `panel recover` (#32) on a cobra root of its own, and
// returns the process's exit code: 0 once the backup is in place, 1 on any
// refusal or error, with nothing written.
func runRecover(args []string) int {
	root := &cobra.Command{Use: "panel", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(recoverCommand())
	root.SetArgs(append([]string{"recover"}, args...))
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func recoverCommand() *cobra.Command {
	options := panel.RecoverOptions{Out: os.Stdout}
	var passphraseFile string

	cmd := &cobra.Command{
		Use:   "recover --kit FILE --from FILE.pcpb",
		Short: "Restores the panel from its backup and recovery kit",
		Long: `Restores a panel lost with its machine: it opens the recovery kit, checks the
backup with the kit's master key as it decrypts it next to pb_data, checks
the database (SQLite's integrity check, migrations this panel knows, the CA
opens with the kit's key), and only then writes the master key file and
puts pb_data in place. A pb_data that holds a panel is kept with --replace,
never deleted. The first start then finishes the restore.

The passphrase comes from --passphrase-file, ` + panel.EnvKitPassphrase + `, or a
prompt. Do not run panel keygen on the new machine. See docs/disaster-recovery.md.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			passphrase, err := panel.ReadPassphrase(passphraseFile, os.Stdin, os.Stderr)
			if err != nil {
				return err
			}
			options.Passphrase = passphrase
			return panel.Recover(options)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&options.Kit, "kit", "", "the recovery kit (.pckit)")
	flags.StringVar(&options.From, "from", "", "the panel's backup (.pcpb), as panel-recovery fetch wrote it")
	flags.StringVar(&options.Dir, "dir", envOr(panel.EnvDataDir, "pb_data"), "the pb_data to restore")
	flags.StringVar(&options.MasterKeyFile, "master-key-file", envOr(panel.EnvMasterKeyFile, "master.key"), "where the kit's master key goes")
	flags.StringVar(&passphraseFile, "passphrase-file", "", "a file with the kit's passphrase")
	flags.BoolVar(&options.Replace, "replace", false, "keep a pb_data that holds a panel as <dir>.replaced-<UTC> and restore in its place")
	flags.StringVar(&options.EnvOut, "env-out", "", "write the kit's environment and POCKETCLOUD_EXPECT_CA_PIN to this docker --env-file")
	flags.BoolVar(&options.DryRun, "dry-run", false, "check everything and write nothing")
	return cmd
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
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
