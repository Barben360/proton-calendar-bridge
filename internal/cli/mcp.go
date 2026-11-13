package cli

import "github.com/spf13/cobra"

// newMCP is pcb mcp: for now the group of its token commands (the stdio
// server comes with its own run).
func (a *app) newMCP() *cobra.Command {
	return Group("mcp", "Serve MCP to agents and manage their tokens", a.newMCPToken())
}
