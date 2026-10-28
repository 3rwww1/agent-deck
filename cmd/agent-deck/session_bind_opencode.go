package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleSessionBindOpenCode(args []string) {
	flags := flag.NewFlagSet("bind-opencode", flag.ContinueOnError)
	server := flags.String("server", "", "OpenCode shared service URL")
	id := flags.String("session-id", "", "OpenCode session ID")
	create := flags.Bool("create", false, "Create the selected session if missing")
	if err := flags.Parse(args); err != nil {
		exitCLI(1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := session.BindOpenCodeSession(ctx, *server, *id, *create); err != nil {
		fmt.Fprintf(os.Stderr, "OpenCode session binding failed: %v\n", err)
		exitCLI(1)
	}
}
