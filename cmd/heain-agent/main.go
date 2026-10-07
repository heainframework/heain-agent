// Command heain-agent is the sidecar that connects an app written in any
// language to heain-core: it is heain-sdk, run as a process next to the app
// (Step 5; project doc claude/decision-2026-10-08-heain-agent.md).
//
// It is configured like any heain-sdk app (the HEAIN_* variables: manifest,
// instance, core, CA, state directory, enrolment) plus HEAIN_AGENT_CONFIG,
// the agent's own JSON file (local API address, the app's callback URLs, the
// app command). The manifest names app.sdk "heain-agent".
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/heainframework/heain-agent/internal/agent"
	"github.com/heainframework/heain-sdk/heain"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println("heain-agent", heain.AgentVersion)
		return
	}
	path := os.Getenv("HEAIN_AGENT_CONFIG")
	if path == "" {
		path = "heain-agent.json" // in the app's directory
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := heain.StartFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REFUSED: %v\n", err)
		os.Exit(2)
	}
	state := os.Getenv("HEAIN_STATE_DIR")
	if state == "" {
		state = filepath.Join(os.TempDir(), "heain-agent-"+app.Instance)
	}
	ag, err := agent.New(app, cfg, state)
	if err != nil {
		log.Fatal(err)
	}
	runErr := ag.Run(ctx)
	_ = app.Close(context.Background())
	if runErr != nil {
		log.Printf("heain-agent: %v", runErr)
		os.Exit(1)
	}
	log.Printf("heain-agent: deregistered")
}
