package cmd

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestCLIContract pins the command names, argument forms, local flags, flag
// types, and defaults that scripts and users already invoke. New commands and
// flags are additive; changing an existing line requires a compatibility
// decision rather than an incidental Cobra edit.
func TestCLIContract(t *testing.T) {
	root := newRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	got := cliContract(root)
	want := strings.TrimSpace(version1CLIContract)
	if got != want {
		t.Fatalf("CLI contract changed:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

func cliContract(root *cobra.Command) string {
	lines := []string{"command aiusage use=" + root.Use}
	appendFlags := func(path string, flags *pflag.FlagSet) {
		flags.VisitAll(func(flag *pflag.Flag) {
			if flag.Name == "help" {
				return
			}
			lines = append(lines, "flag "+path+" --"+flag.Name+
				" type="+flag.Value.Type()+" default="+flag.DefValue)
		})
	}
	appendFlags("aiusage", root.PersistentFlags())

	for _, command := range root.Commands() {
		path := "aiusage " + command.Name()
		lines = append(lines, "command "+path+" use="+command.Use)
		appendFlags(path, command.LocalNonPersistentFlags())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

const version1CLIContract = `
command aiusage completion use=completion
command aiusage doctor use=doctor
command aiusage export use=export
command aiusage help use=help [command]
command aiusage last use=last <duration>
command aiusage once use=once
command aiusage run use=run
command aiusage setup use=setup
command aiusage sources use=sources
command aiusage summary use=summary
command aiusage today use=today
command aiusage use=aiusage
command aiusage version use=version
flag aiusage --config type=string default=
flag aiusage --db type=string default=
flag aiusage --home type=string default=
flag aiusage --interval type=int default=0
flag aiusage --no-daemon type=bool default=false
flag aiusage export --format type=string default=json
flag aiusage export --include-raw type=bool default=false
flag aiusage export --out type=string default=
flag aiusage export --since type=string default=
flag aiusage export --until type=string default=
flag aiusage last --json type=bool default=false
flag aiusage setup --force type=bool default=false
flag aiusage setup --remove type=bool default=false
flag aiusage summary --breakdown type=bool default=false
flag aiusage summary --by type=string default=
flag aiusage summary --csv type=bool default=false
flag aiusage summary --json type=bool default=false
flag aiusage summary --since type=string default=
flag aiusage summary --until type=string default=
flag aiusage today --json type=bool default=false
`
