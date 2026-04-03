package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "curlycatclaw-gws-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	gwsPath := os.Getenv("GWS_PATH")
	if gwsPath == "" {
		gwsPath = "gws"
	}

	filterStr := os.Getenv("GWS_FILTER")

	server := mcp.NewServer(
		&mcp.Implementation{Name: "curlycatclaw-gws-mcp", Version: version},
		nil,
	)

	exec := &Executor{GWSPath: gwsPath}

	// Discover tools from gws generate-skills output.
	skillsDir := os.Getenv("GWS_SKILLS_DIR")
	count, err := discoverAndRegister(server, exec, gwsPath, skillsDir, filterStr)
	if err != nil {
		slog.Warn("gws-mcp: skill discovery failed, generic tool only", "err", err)
	}

	// Always register the generic API escape hatch.
	registerGenericTool(server, exec)

	slog.Info("gws-mcp: starting", "discovered_tools", count, "gws_path", gwsPath)

	return server.Run(context.Background(), &mcp.StdioTransport{})
}
