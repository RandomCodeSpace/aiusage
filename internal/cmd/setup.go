package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/service"
)

// noSupervisionNotice is what setup prints on a machine with no systemd user
// manager. It is not an error: the fallback works, it just does not survive a
// reboot on its own, and the user asked what the state of things is.
const noSupervisionNotice = "no systemd user session here, so there is nothing to install.\n" +
	"aiusage keeps starting its collector as a detached background process, which\n" +
	"collects exactly the same data but does not come back by itself after a reboot."

// newSetupCmd builds the `setup` command: the explicit, inspectable half of the
// supervision that data-facing commands otherwise arrange by themselves.
//
// It exists for three jobs the automatic path deliberately will not do: install
// with flags baked in (--db and friends, which the automatic path refuses to
// guess at), repair a unit somebody edited into a corner (--force), and undo
// the whole thing (--remove). Everything it does is idempotent, so running it
// twice is the same as running it once.
func newSetupCmd() *cobra.Command {
	var (
		remove bool
		force  bool
	)
	c := &cobra.Command{
		Use:   "setup",
		Short: "Install the aiusage systemd user service",
		Long: "setup writes, enables and starts the systemd USER unit aiusage runs " +
			"under - the collection daemon - in $XDG_CONFIG_HOME/systemd/user. No " +
			"root is needed and none is asked for.\n\n" +
			"It is create-if-missing: a unit file that already exists is left " +
			"exactly as it is, because it may carry your edits, and only its " +
			"enabled and running state is corrected. Use --force to replace it " +
			"anyway, and --remove to stop, disable and delete it.\n\n" +
			"Any global flag you pass (--db, --config, --home, --interval) is " +
			"written into the unit. That is the difference between this command " +
			"and the install that data-facing commands do on their own: asking is " +
			"consent, so setup bakes what it was given, while the automatic path " +
			"skips the install entirely rather than make a scratch database " +
			"permanent. --no-daemon suppresses that automatic install; it does not " +
			"suppress this command, which is the explicit ask.\n\n" +
			"--remove deletes only a unit file aiusage generated, which it knows by " +
			"the stamp comment it writes into it. A unit you wrote yourself is " +
			"named and left alone unless --force says otherwise.\n\n" +
			"On a machine with no systemd user session, setup says so and changes " +
			"nothing; aiusage falls back to a detached background collector.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSetup(c, setupFlags{remove: remove, force: force})
		},
	}
	f := c.Flags()
	f.BoolVar(&remove, "remove", false, "stop, disable and delete the unit instead of installing it")
	f.BoolVar(&force, "force", false,
		"rewrite a unit file that already exists, and with --remove delete a unit file "+
			"aiusage did not generate (discards local edits)")
	return c
}

// setupFlags carries the setup command's own flags.
type setupFlags struct {
	remove bool
	force  bool
}

// setupBudget bounds the WHOLE explicit setup, install or removal.
//
// It is not supervisionBudget and is deliberately much larger. That one sits in
// front of a command the user typed to see a number, where five seconds spent
// on systemd is five seconds stolen; this one IS the command, the user asked
// for it and is watching it happen, and abandoning an install between writing a
// unit file and starting it leaves the machine half configured for the sake of
// a few seconds nobody was waiting on. What it may still not do is hang without
// end, so a manager that stops answering ends the attempt with everything that
// did happen already printed and an error naming the deadline.
//
// It is a var only so tests can shrink it; nothing else assigns to it.
var setupBudget = 30 * time.Second

func runSetup(c *cobra.Command, sf setupFlags) error {
	out := c.OutOrStdout()
	ctx, cancel := context.WithTimeout(cmdContext(c), setupBudget)
	defer cancel()

	m := newSupervisor()
	if !m.Available(ctx) {
		fmt.Fprintln(out, noSupervisionNotice)
		return nil
	}

	if sf.remove {
		res, err := m.Remove(ctx, sf.force)
		printLines(out, res.Lines)
		if err != nil {
			return err
		}
		// A refusal is not a failure - nothing broke, and the files are exactly
		// where the user left them - but it is not success either. Exiting zero
		// here tells `aiusage setup --remove && rm -rf ~/.config/systemd/user`
		// that the directory is clean while a unit file is still in it.
		if res.Refused > 0 {
			return fmt.Errorf("left %d unit file(s) in place: aiusage did not write them; pass --force to delete them anyway",
				res.Refused)
		}
		return nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	o, err := supervisionOptions(cfg, globalArgs(flags))
	if err != nil {
		return err
	}
	printEnvNotice(out, flags)
	o.Force = sf.force

	res, err := m.Install(ctx, o)
	printLines(out, res.Lines)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "aiusage is supervised by systemd; check it with: systemctl --user status %s\n", service.CollectUnit)
	return nil
}

// printEnvNotice warns that this shell's environment moved something the units
// will not inherit.
//
// setup bakes the FLAGS it was given, and there is no flag for most of what the
// environment can move (a state directory, a config directory, an adapter's
// discovery root), so a unit installed under AIUSAGE_DB or XDG_STATE_HOME gets
// those directories in its ReadWritePaths while its ExecStart resolves the
// defaults, and one installed under CLAUDE_CONFIG_DIR collects from the default
// Claude directory. The automatic install refuses both cases outright; an
// explicit setup is consent, so it installs and says the one thing the user
// needs to hear. Nothing is printed about a path the equivalent flag already
// carried, since a flag does land in the unit.
func printEnvNotice(out io.Writer, f globalFlags) {
	if env := config.PathEnvOverrides(); len(env) > 0 && !(f.db != "" && f.home != "" && f.config != "") {
		fmt.Fprintf(out, "note: %s moved a path in this shell, and a unit does not inherit the environment; "+
			"pass the equivalent flag if the services must use it\n", strings.Join(env, ", "))
	}
	if env := discoveryEnvOverrides(); len(env) > 0 {
		fmt.Fprintf(out, "note: %s moved an adapter's discovery root in this shell, and a unit does not "+
			"inherit the environment; the services will read the default locations\n", strings.Join(env, ", "))
	}
}

// printLines writes an operation's account, one plain line each. The lines are
// written verbatim rather than as format strings: they carry paths, and a path
// with a percent sign in it is not a format verb.
func printLines(out io.Writer, lines []string) {
	for _, ln := range lines {
		fmt.Fprintln(out, ln)
	}
}
