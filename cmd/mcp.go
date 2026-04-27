package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/itsHabib/orchestra/internal/mcp"
)

// MCP transport selectors. The const names match the user-facing flag values
// — `--transport stdio` / `--transport http` — so misspellings get caught
// at flag-parse time via the cobra validator.
const (
	mcpTransportStdio = "stdio"
	mcpTransportHTTP  = "http"
)

const defaultMCPHTTPAddress = "127.0.0.1:7332"

var (
	mcpTransportFlag    string
	mcpHTTPAddressFlag  string
	mcpRegistryPathFlag string
	mcpWorkspaceRoot    string
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the orchestra MCP server (parent-Claude entry point)",
	Long: "Long-running MCP server that exposes ship_design_docs / list_jobs / " +
		"get_status / unblock to a parent Claude Code session. Default transport " +
		"is stdio (matches Claude Code's MCP client attachment shape); --transport " +
		"http exposes a Streamable HTTP listener with NO authentication and is " +
		"intended for trusted-host advanced use only.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runMCP(cmd.Context())
	},
}

func runMCP(parentCtx context.Context) error {
	switch mcpTransportFlag {
	case mcpTransportStdio, mcpTransportHTTP:
	default:
		return fmt.Errorf("--transport must be %q or %q, got %q",
			mcpTransportStdio, mcpTransportHTTP, mcpTransportFlag)
	}

	opts := &mcp.Options{}
	if mcpRegistryPathFlag != "" {
		opts.Registry = mcp.NewRegistry(mcpRegistryPathFlag)
	}
	if mcpWorkspaceRoot != "" {
		opts.WorkspaceRoot = mcpWorkspaceRoot
	}
	srv, err := mcp.New(opts)
	if err != nil {
		return fmt.Errorf("init mcp server: %w", err)
	}

	ctx, stop := signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch mcpTransportFlag {
	case mcpTransportStdio:
		// Stdio: the only sane logging channel is stderr — stdout is the
		// JSON-RPC pipe. No banner; clients don't need it.
		if err := srv.ServeStdio(ctx); err != nil {
			return fmt.Errorf("mcp stdio: %w", err)
		}
		return nil
	case mcpTransportHTTP:
		if mcpHTTPAddressFlag == "" {
			return errors.New("--addr is required when --transport http")
		}
		fmt.Fprintf(os.Stderr,
			"WARNING: --transport http exposes the MCP server with NO "+
				"authentication on %s. Run only on trusted hosts; HTTP "+
				"auth is deferred to a follow-up PR.\n",
			mcpHTTPAddressFlag)
		if err := srv.ServeHTTP(ctx, mcpHTTPAddressFlag); err != nil {
			return fmt.Errorf("mcp http: %w", err)
		}
		return nil
	}
	return nil
}

func init() {
	mcpCmd.Flags().StringVar(&mcpTransportFlag, "transport", mcpTransportStdio,
		"MCP transport: \"stdio\" (default, for Claude Code) or \"http\".")
	mcpCmd.Flags().StringVar(&mcpHTTPAddressFlag, "addr", defaultMCPHTTPAddress,
		"Listen address for --transport http. Loopback by default.")
	mcpCmd.Flags().StringVar(&mcpRegistryPathFlag, "registry-path", "",
		"Override the run registry file path. Defaults to platform user data dir.")
	mcpCmd.Flags().StringVar(&mcpWorkspaceRoot, "workspace-root", "",
		"Override the per-run workspace parent directory. Defaults to platform user data dir.")
}
