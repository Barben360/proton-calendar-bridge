package cli_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
)

// docsFlagRefs reads docs/commands.md the way it is written: the tables of
// "## The commands" have one row per way to run a command, whose first column
// holds `pcb ...` code spans (the command path, then its flags) and whose
// second column may name more flags in code spans. A command's documented
// flags are the union over its rows.
//
// Exceptions, each with its reason:
//   - root's persistent flags (--json, --account, --non-interactive) are
//     documented once for every command (JSON output, Choose the account, the
//     login row), never per command, so they are skipped per command and
//     checked to be mentioned in the page at all;
//   - a command with subcommands has no row of its own required (account,
//     calendar, event, client, service and mcp token are only groups), and its
//     own flags count as documented when a row of any command below it names
//     them (service's --system and --user);
//   - the flags in undocumentedOnPurpose, each with its reason;
//   - hidden commands and hidden flags are not documented by design;
//   - cobra adds help and completion only when it executes, so the tree built
//     here has none to skip.
//
// Flags named in a second column only count as documented; a first-column flag
// that does not exist on its command is an error.
func TestDocsCommandsFlagTablesMatchTheCommands(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	root := cli.NewRoot()
	problems := checkFlagTables(root, doc)
	for _, p := range problems {
		t.Error(p)
	}
}

// checkFlagTables returns one message per drift between root's tree and doc.
func checkFlagTables(root *cobra.Command, doc string) []string {
	var problems []string
	globals := map[string]bool{}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		globals[f.Name] = true
		if !strings.Contains(doc, "--"+f.Name) {
			problems = append(problems, fmt.Sprintf("global flag --%s is not documented anywhere in docs/commands.md", f.Name))
		}
	})

	rows := commandRows(doc)
	documented := map[*cobra.Command]map[string]bool{} // coverage: first and second column
	hasRow := map[*cobra.Command]bool{}
	note := func(c *cobra.Command, name string) {
		if documented[c] == nil {
			documented[c] = map[string]bool{}
		}
		documented[c][name] = true
	}
	for _, row := range rows {
		for _, span := range row.commands {
			targets, flags, err := resolveSpan(root, span)
			if err != nil {
				problems = append(problems, fmt.Sprintf("docs row %q: %v", span, err))
				continue
			}
			for _, c := range targets {
				hasRow[c] = true
			}
			for _, fl := range flags {
				if globals[fl.name] && fl.isLong {
					continue
				}
				found := false
				for _, c := range targets {
					if f := lookupFlag(c, fl); f != nil {
						found = true
						note(c, f.Name)
					}
				}
				if !found {
					problems = append(problems, fmt.Sprintf("docs row %q: flag %s does not exist on %s", span, fl, pathOf(targets)))
				}
			}
			// Second-column flags credit the row's commands only.
			for _, fl := range row.descFlags {
				for _, c := range targets {
					if f := lookupFlag(c, fl); f != nil {
						note(c, f.Name)
					}
				}
			}
		}
	}

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		if c != root {
			name := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
			if len(c.Commands()) == 0 && c.Runnable() && !hasRow[c] {
				problems = append(problems, fmt.Sprintf("command %q has no row in docs/commands.md", name))
			}
			c.LocalFlags().VisitAll(func(f *pflag.Flag) {
				if _, ok := undocumentedOnPurpose[c.CommandPath()][f.Name]; ok || f.Hidden || f.Name == "help" || globals[f.Name] && root.PersistentFlags().Lookup(f.Name) == f {
					return
				}
				if !documentedBelow(c, f.Name, documented) {
					problems = append(problems, fmt.Sprintf("command %q: flag --%s is not in the docs/commands.md table", name, f.Name))
				}
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	sort.Strings(problems)
	return problems
}

// documentedBelow reports whether flag name is documented for c, or for a
// command below c when c is a group.
func documentedBelow(c *cobra.Command, name string, documented map[*cobra.Command]map[string]bool) bool {
	if documented[c][name] {
		return true
	}
	for _, sub := range c.Commands() {
		if documentedBelow(sub, name, documented) {
			return true
		}
	}
	return false
}

func pathOf(cs []*cobra.Command) string {
	var names []string
	for _, c := range cs {
		names = append(names, "`"+c.CommandPath()+"`")
	}
	return strings.Join(names, ", ")
}

// docFlag is a flag as a row writes it: --name or -x.
type docFlag struct {
	name   string // without dashes
	isLong bool
}

func (f docFlag) String() string {
	if f.isLong {
		return "--" + f.name
	}
	return "-" + f.name
}

// lookupFlag finds fl among c's own flags, then those a group above it passes
// down (service's --user and --system).
func lookupFlag(c *cobra.Command, fl docFlag) *pflag.Flag {
	for _, fs := range []*pflag.FlagSet{c.LocalFlags(), c.InheritedFlags()} {
		if fl.isLong {
			if f := fs.Lookup(fl.name); f != nil {
				return f
			}
		} else if f := fs.ShorthandLookup(fl.name); f != nil {
			return f
		}
	}
	return nil
}

// undocumentedOnPurpose lists the flags a command registers and refuses, so
// the docs table leaves them out (command path, then flag name).
var undocumentedOnPurpose = map[string]map[string]string{
	"pcb login resume": {
		"username":           "login's own flag, registered on resume to be refused: the daemon holds the login it made",
		"password-fd":        "login's own flag, registered on resume to be refused: the daemon holds the login it made",
		"unattended-relogin": "login's own flag, registered on resume to be refused: the daemon holds the login it made",
	},
}

type docRow struct {
	commands  []string  // the code spans of the first column that start a pcb command
	descFlags []docFlag // flags in code spans of the second column
}

var (
	codeSpan  = regexp.MustCompile("`([^`]+)`")
	flagLong  = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
	flagShort = regexp.MustCompile(`^-[a-zA-Z]$`)
	// flagSeparators turns the brackets and bars of `[--a x|y]` into spaces.
	flagSeparators = strings.NewReplacer("[", " ", "]", " ", "(", " ", ")", " ", "|", " ", ",", " ")
	wordToken      = regexp.MustCompile(`^[a-z][a-z-]*$`)
)

// commandRows returns the rows of the tables between "## The commands" and the
// next "## " heading.
func commandRows(doc string) []docRow {
	var rows []docRow
	in := false
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			in = line == "## The commands"
		case in && strings.HasPrefix(line, "| "):
			cells := splitCells(line)
			if len(cells) < 2 || cells[0] == "Command" || strings.HasPrefix(cells[0], "---") {
				continue
			}
			var r docRow
			for _, m := range codeSpan.FindAllStringSubmatch(cells[0], -1) {
				if strings.Contains(" "+m[1], " pcb ") || m[1] == "pcb" {
					r.commands = append(r.commands, m[1])
				}
			}
			for _, m := range codeSpan.FindAllStringSubmatch(cells[1], -1) {
				r.descFlags = append(r.descFlags, flagsIn(m[1])...)
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// splitCells splits a table row on its unescaped pipes and unescapes the \|.
func splitCells(line string) []string {
	line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func flagsIn(s string) []docFlag {
	var out []docFlag
	for _, w := range strings.Fields(flagSeparators.Replace(s)) {
		w, _, _ = strings.Cut(w, "=")
		switch {
		case flagLong.MatchString(w):
			out = append(out, docFlag{name: w[2:], isLong: true})
		case flagShort.MatchString(w):
			out = append(out, docFlag{name: w[1:]})
		}
	}
	return out
}

// resolveSpan walks a `[ENV=v ...] pcb a b|c [args] [--flags]` span down the
// tree: the commands it runs (several for `a|b` alternatives) and its flags.
func resolveSpan(root *cobra.Command, span string) ([]*cobra.Command, []docFlag, error) {
	words := strings.Fields(span)
	start := -1
	for i, w := range words {
		if w == root.Name() {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, nil, fmt.Errorf("no %s command", root.Name())
	}
	words = words[start+1:]
	targets := []*cobra.Command{root}
	rest := 0
	for rest < len(words) {
		w := words[rest]
		if !wordToken.MatchString(strings.ReplaceAll(w, "|", "-")) {
			break // an operand, an option or a flag
		}
		var next []*cobra.Command
		for _, part := range strings.Split(w, "|") {
			var hit *cobra.Command
			for _, c := range targets {
				for _, sub := range c.Commands() {
					if sub.Name() == part {
						hit = sub
					}
				}
			}
			if hit != nil {
				next = append(next, hit)
			}
		}
		if len(next) == 0 {
			// An operand such as on|off|status, unless the command takes
			// subcommands: then it is a command that does not exist.
			if len(targets[0].Commands()) > 0 {
				return nil, nil, fmt.Errorf("%q is not a command of %s", w, targets[0].CommandPath())
			}
			break
		}
		if len(next) != len(strings.Split(w, "|")) {
			return nil, nil, fmt.Errorf("%q names a command that does not exist under %s", w, targets[0].CommandPath())
		}
		targets = next
		rest++
	}
	return targets, flagsIn(strings.Join(words[rest:], " ")), nil
}
