package main

import (
	"fmt"
	"os"

	"github.com/punklabs-ai/falkn/internal/buildinfo"
	"github.com/punklabs-ai/falkn/internal/notifications"
	"github.com/punklabs-ai/falkn/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "falknd:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		printUsage()
		return nil
	}
	switch arguments[0] {
	case "serve":
		if len(arguments) != 1 {
			return fmt.Errorf("serve does not accept arguments")
		}
		return server.New(buildinfo.Version).Serve()
	case "rpc":
		if len(arguments) != 2 {
			return fmt.Errorf("usage: falknd rpc <base64-json-request>")
		}
		return server.Call(arguments[1], os.Stdout)
	case "notify-watch":
		if len(arguments) != 1 {
			return fmt.Errorf("notify-watch does not accept arguments")
		}
		paths, err := server.RuntimePaths()
		if err != nil {
			return err
		}
		return notifications.Watch(notifications.WatcherPaths{
			Socket: paths.Socket, Config: paths.NotificationConfig,
			State: paths.NotificationState, Lock: paths.NotificationLock,
			Activity: paths.NotificationActivity, Presence: paths.NotificationPresence,
			Follow: paths.NotificationFollow,
		})
	case "version", "--version", "-v":
		fmt.Printf("falknd %s\n", buildinfo.Version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func printUsage() {
	fmt.Println("falknd - Falkn's persistent coding-agent runtime")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  falknd rpc <base64-json-request>  Send one local RPC request")
	fmt.Println("  falknd serve                      Run the background server")
	fmt.Println("  falknd version                    Print version information")
}
