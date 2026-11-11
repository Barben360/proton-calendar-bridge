// Package clitest checks the CLI contract every command tree must honour
// (SPEC §13), so each binary tests itself with one call.
package clitest

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// Paths lists the command path (without the binary) of every command of root.
func Paths(root *cobra.Command) [][]string {
	var out [][]string
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		out = append(out, path)
		for _, sub := range c.Commands() {
			walk(sub, append(append([]string(nil), path...), sub.Name()))
		}
	}
	walk(root, nil)
	return out
}

// CheckUsageContract runs a bad invocation of every command path of the tree
// newRoot builds: each must exit 2 with exactly one envelope on stdout,
// nothing on stderr, and the command's own path as its name.
func CheckUsageContract(t *testing.T, newRoot func() *cobra.Command) {
	t.Helper()
	for _, path := range Paths(newRoot()) {
		name := strings.Join(path, " ")
		cases := map[string][]string{
			"unknown flag":       {"--bogus-flag"},
			"unexpected operand": {"nope"},
		}
		if len(path) == 0 || cli.IsGroup(find(newRoot(), path)) {
			cases["no subcommand"] = nil
		}
		for what, extra := range cases {
			t.Run(strings.TrimSpace("pcb "+name+" / "+what), func(t *testing.T) {
				args := append(append(append([]string(nil), path...), extra...), "--json")
				var out, errb bytes.Buffer
				code := cli.Run(newRoot(), args, &out, &errb)
				if code != exitcode.Usage {
					t.Fatalf("args %v: exit code %d, want %d (stdout %q)", args, code, exitcode.Usage, out.String())
				}
				if errb.Len() != 0 {
					t.Errorf("args %v: stderr = %q, want empty in JSON mode", args, errb.String())
				}
				var env struct {
					Command string `json:"command"`
					OK      bool   `json:"ok"`
					Error   *struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				dec := json.NewDecoder(&out)
				if err := dec.Decode(&env); err != nil {
					t.Fatalf("args %v: stdout is not an envelope: %v", args, err)
				}
				if _, err := dec.Token(); err != io.EOF {
					t.Errorf("args %v: more than one JSON value on stdout", args)
				}
				if env.OK || env.Error == nil || env.Error.Code != int(exitcode.Usage) {
					t.Errorf("args %v: envelope = %+v, want a code 2 failure", args, env)
				}
				if what == "unexpected operand" && env.Error != nil && !strings.Contains(env.Error.Message, "nope") {
					t.Errorf("args %v: message %q does not name the rejected operand", args, env.Error.Message)
				}
				if want := commandName(newRoot(), path); env.Command != want {
					t.Errorf("args %v: command = %q, want %q", args, env.Command, want)
				}
			})
		}
	}
}

func find(root *cobra.Command, path []string) *cobra.Command {
	c := root
	for _, p := range path {
		for _, sub := range c.Commands() {
			if sub.Name() == p {
				c = sub
				break
			}
		}
	}
	return c
}

// commandName is the name an envelope must carry: the path, or the binary
// for the root. Spelled out here so the test does not reuse the code it checks.
func commandName(root *cobra.Command, path []string) string {
	if len(path) == 0 {
		return root.Name()
	}
	return strings.Join(path, " ")
}
