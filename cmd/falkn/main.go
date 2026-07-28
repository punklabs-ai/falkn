package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/punklabs-ai/falkn/internal/buildinfo"
	"github.com/punklabs-ai/falkn/internal/notifications"
	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "falkn:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return open(false, "")
	}
	switch arguments[0] {
	case "new":
		if len(arguments) > 2 {
			return errors.New("usage: falkn new [name]")
		}
		title := ""
		if len(arguments) == 2 {
			title = arguments[1]
		}
		return open(true, title)
	case "attach":
		if len(arguments) != 2 {
			return errors.New("usage: falkn attach <session-id>")
		}
		return attach(arguments[1])
	case "list", "ls":
		if len(arguments) != 1 {
			return errors.New("list does not accept arguments")
		}
		return list()
	case "notifications":
		return notificationsCommand(arguments[1:])
	case "version", "--version", "-v":
		fmt.Printf("falkn %s\n", buildinfo.Version)
		return nil
	case "daemon":
		return daemonCommand(arguments[1:])
	case "help", "--help", "-h":
		printUsage()
		return nil
	case "serve":
		return server.New(buildinfo.Version).Serve()
	case "rpc":
		if len(arguments) != 2 {
			return errors.New("usage: falkn rpc <base64-json-request>")
		}
		return server.Call(arguments[1], os.Stdout)
	case "notify-watch":
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
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func open(forceNew bool, requestedTitle string) error {
	// Reject an unusable hotkey prefix before a session is created for it.
	if _, err := server.AttachPrefix(); err != nil {
		return err
	}
	directory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("find current directory: %w", err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve current directory: %w", err)
	}

	preflight, listed, err := daemonState()
	if err != nil {
		return err
	}
	shellSupported := supportsFeature(preflight, "shell")
	currentSessionID := os.Getenv("FALKN_SESSION_ID")
	if !forceNew {
		if session := reusableShellSession(listed.Sessions, directory, currentSessionID); session != nil {
			err := attachOrHandOff(listed.Sessions, currentSessionID, session.ID)
			// A session listed a moment ago can finish before it is attached,
			// which is a reason to open a new shell rather than to report that
			// the one Falkn chose by itself has gone.
			if !server.SessionGone(err) {
				return err
			}
		}
	}
	if !shellSupported {
		running := runningSessions(listed.Sessions)
		if len(running) > 0 {
			return incompatibleShellDaemonError(preflight, running)
		}
		preflight, err = restartDaemon(preflight, listed.Sessions)
		if err != nil {
			return fmt.Errorf("upgrade incompatible falknd: %w", err)
		}
		if !supportsFeature(preflight, "shell") {
			return fmt.Errorf("restarted falknd %s still does not support local Falkn shells", preflight.DaemonVersion)
		}
	}

	title := strings.TrimSpace(requestedTitle)
	if title == "" {
		title = filepath.Base(directory)
		if title == "." || title == string(filepath.Separator) || title == "" {
			title = "Falkn shell"
		}
	}
	var created protocol.CreateResult
	if err := call("shell.create", protocol.ShellCreateParams{
		Title: title, Directory: directory, TerminalColumns: 80, TerminalRows: 24,
	}, &created); err != nil {
		return err
	}
	return attachOrHandOff(listed.Sessions, currentSessionID, created.Session.ID)
}

// attachOrHandOff opens a session in the terminal that is already attached to
// one, rather than inside it. Nesting a second client in a session's own
// terminal buries the outer session and doubles every keystroke's path; running
// falkn from inside a session is a request to move to another one, not to layer
// them.
func attachOrHandOff(sessions []protocol.SessionRecord, currentSessionID, targetSessionID string) error {
	if shouldRequestAttachHandoff(sessions, currentSessionID, targetSessionID) {
		return server.WriteAttachHandoff(os.Stdout, targetSessionID)
	}
	return runAttachClient(targetSessionID)
}

func attach(sessionID string) error {
	preflight, listed, err := daemonState()
	if err != nil {
		return err
	}
	currentSessionID := os.Getenv("FALKN_SESSION_ID")
	if currentSessionID == sessionID {
		return fmt.Errorf("already attached to session %s", sessionID)
	}
	if supportsFeature(preflight, "attach") || isShellSession(listed.Sessions, sessionID) {
		return attachOrHandOff(listed.Sessions, currentSessionID, sessionID)
	}
	return fmt.Errorf(
		"running falknd %s predates CLI attachment; session %s remains available in the Falkn mobile app",
		preflight.DaemonVersion,
		sessionID,
	)
}

func runAttachClient(sessionID string) error {
	err := server.AttachClient(sessionID, os.Stdin, os.Stdout)
	var handoff *server.AttachHandoffError
	if !errors.As(err, &handoff) {
		return err
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		return fmt.Errorf("find Falkn executable for attach handoff: %w", executableErr)
	}
	return syscall.Exec(
		executable,
		[]string{executable, "attach", handoff.SessionID},
		os.Environ(),
	)
}

func shouldRequestAttachHandoff(
	sessions []protocol.SessionRecord,
	currentSessionID, targetSessionID string,
) bool {
	if currentSessionID == "" || currentSessionID == targetSessionID {
		return false
	}
	for _, session := range sessions {
		if session.ID == currentSessionID {
			return session.Status == "running" && session.AttachedClients > 0
		}
	}
	return false
}

// notificationsCommand shows the devices this machine sends events to and lets
// one of them go. Every registration listed here is another copy of every
// notification, and a device that was reinstalled leaves its old credentials
// behind with no way to withdraw them itself.
func notificationsCommand(arguments []string) error {
	paths, err := server.RuntimePaths()
	if err != nil {
		return err
	}
	switch {
	case len(arguments) == 0:
		return listNotificationDevices(paths.NotificationConfig)
	case arguments[0] == "forget" && len(arguments) == 2:
		return forgetNotificationDevice(paths.NotificationConfig, arguments[1])
	default:
		return errors.New("usage: falkn notifications [forget <device-id|all>]")
	}
}

func listNotificationDevices(path string) error {
	configuration, err := notifications.LoadConfiguration(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(configuration.Registrations) == 0) {
		fmt.Println("This machine does not send notifications to any device.")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println("DEVICE\tHOST\tLAST SEEN\tEVENTS")
	for _, registration := range configuration.Registrations {
		lastSeen := "unknown"
		if registration.UpdatedAt != 0 {
			lastSeen = time.Unix(registration.UpdatedAt, 0).Format("2006-01-02 15:04")
		}
		events := "all"
		if registration.EnabledStates != nil {
			names := make([]string, 0, len(registration.EnabledStates))
			for _, state := range registration.EnabledStates {
				names = append(names, string(state))
			}
			events = strings.Join(names, ",")
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", registration.DeviceID, registration.HostName, lastSeen, events)
	}
	return nil
}

func forgetNotificationDevice(path, deviceID string) error {
	if deviceID == "all" {
		deviceID = ""
	}
	forgotten, err := notifications.Forget(path, deviceID)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("This machine does not send notifications to any device.")
		return nil
	}
	if err != nil {
		return err
	}
	if forgotten == 0 {
		return fmt.Errorf("no device is registered as %s", deviceID)
	}
	fmt.Printf("Forgot %d device registration(s). The Falkn app registers again the next time it opens.\n", forgotten)
	return nil
}

func daemonCommand(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: falkn daemon <status|restart>")
	}
	switch arguments[0] {
	case "status":
		return daemonStatus()
	case "restart":
		return daemonRestart()
	default:
		return fmt.Errorf("unknown daemon command %q", arguments[0])
	}
}

func daemonStatus() error {
	preflight, err := daemonPreflight()
	if err != nil {
		return err
	}
	var listed protocol.ListResult
	if err := call("list", protocol.EmptyParams{}, &listed); err != nil {
		return err
	}
	running := runningSessions(listed.Sessions)
	fmt.Printf("Client:  %s\n", buildinfo.Version)
	fmt.Printf("Daemon:  %s\n", preflight.DaemonVersion)
	fmt.Printf("Running: %d session(s)\n", len(running))
	if preflight.DaemonVersion != buildinfo.Version || !supportsFeature(preflight, "shell") {
		fmt.Println("Update pending: run `falkn daemon restart` after active sessions finish.")
	}
	return nil
}

func daemonRestart() error {
	preflight, listed, err := daemonState()
	if err != nil {
		return err
	}
	restarted, err := restartDaemon(preflight, listed.Sessions)
	if err != nil {
		return err
	}
	fmt.Printf("Restarted falknd %s.\n", restarted.DaemonVersion)
	return nil
}

func restartDaemon(preflight protocol.PreflightResult, sessions []protocol.SessionRecord) (protocol.PreflightResult, error) {
	running := runningSessions(sessions)
	if len(running) > 0 {
		return protocol.PreflightResult{}, fmt.Errorf(
			"cannot restart falknd while sessions are running: %s",
			strings.Join(sessionNames(running), ", "),
		)
	}
	if supportsFeature(preflight, "daemon_shutdown") {
		var acknowledgement protocol.AckResult
		if err := call("daemon.shutdown", protocol.EmptyParams{}, &acknowledgement); err != nil {
			return protocol.PreflightResult{}, err
		}
		if !acknowledgement.Accepted {
			return protocol.PreflightResult{}, errors.New("falknd did not accept the restart request")
		}
	} else {
		var confirmed protocol.ListResult
		if err := call("list", protocol.EmptyParams{}, &confirmed); err != nil {
			return protocol.PreflightResult{}, fmt.Errorf("recheck legacy falknd sessions: %w", err)
		}
		if running := runningSessions(confirmed.Sessions); len(running) > 0 {
			return protocol.PreflightResult{}, fmt.Errorf(
				"cannot restart falknd while sessions are running: %s",
				strings.Join(sessionNames(running), ", "),
			)
		}
		if _, err := server.StopDaemonProcess(); err != nil {
			return protocol.PreflightResult{}, fmt.Errorf("stop legacy falknd %s: %w", preflight.DaemonVersion, err)
		}
	}
	paths, err := server.RuntimePaths()
	if err != nil {
		return protocol.PreflightResult{}, err
	}
	deadline := time.Now().Add(3 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(paths.Socket); errors.Is(statErr, os.ErrNotExist) {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		return protocol.PreflightResult{}, errors.New("falknd did not stop within 3 seconds")
	}
	restarted, err := daemonPreflight()
	if err != nil {
		return protocol.PreflightResult{}, fmt.Errorf("start updated falknd: %w", err)
	}
	return restarted, nil
}

func daemonPreflight() (protocol.PreflightResult, error) {
	var result protocol.PreflightResult
	err := call("preflight", protocol.EmptyParams{}, &result)
	return result, err
}

func daemonState() (protocol.PreflightResult, protocol.ListResult, error) {
	preflight, err := daemonPreflight()
	if err != nil {
		return protocol.PreflightResult{}, protocol.ListResult{}, err
	}
	var listed protocol.ListResult
	if err := call("list", protocol.EmptyParams{}, &listed); err != nil {
		return protocol.PreflightResult{}, protocol.ListResult{}, err
	}
	return preflight, listed, nil
}

func supportsFeature(preflight protocol.PreflightResult, feature string) bool {
	for _, candidate := range preflight.Features {
		if candidate == feature {
			return true
		}
	}
	return false
}

func runningSessions(sessions []protocol.SessionRecord) []protocol.SessionRecord {
	running := make([]protocol.SessionRecord, 0, len(sessions))
	for _, session := range sessions {
		if session.Status == "running" {
			running = append(running, session)
		}
	}
	return running
}

// reusableShellSession finds the shell already open on a directory, never
// counting the session the caller is running inside. That session matches its
// own directory, and attaching it to itself feeds its output back into its own
// terminal, which never settles.
func reusableShellSession(
	sessions []protocol.SessionRecord,
	directory, currentSessionID string,
) *protocol.SessionRecord {
	for index := range sessions {
		session := &sessions[index]
		if session.ID == currentSessionID {
			continue
		}
		if session.Status == "running" && session.Kind == "shell" && samePath(session.Directory, directory) {
			return session
		}
	}
	return nil
}

func isShellSession(sessions []protocol.SessionRecord, sessionID string) bool {
	for _, session := range sessions {
		if session.ID == sessionID {
			return session.Kind == "shell"
		}
	}
	return false
}

func incompatibleShellDaemonError(preflight protocol.PreflightResult, running []protocol.SessionRecord) error {
	return fmt.Errorf(
		"running falknd %s does not support local shells and is preserving active sessions: %s; they remain available in the Falkn mobile app—finish them there, then run falkn again to upgrade automatically",
		preflight.DaemonVersion,
		strings.Join(sessionNames(running), ", "),
	)
}

func sessionNames(sessions []protocol.SessionRecord) []string {
	names := make([]string, 0, len(sessions))
	for _, session := range sessions {
		names = append(names, fmt.Sprintf("%s (%s)", session.Title, session.ID))
	}
	return names
}

func list() error {
	var result protocol.ListResult
	if err := call("list", protocol.EmptyParams{}, &result); err != nil {
		return err
	}
	if len(result.Sessions) == 0 {
		fmt.Println("No Falkn sessions.")
		return nil
	}
	fmt.Println("ID\tSTATE\tAGENT\tDIRECTORY\tNAME")
	for _, session := range result.Sessions {
		agentID := session.AgentID
		if agentID == "shell" {
			agentID = "-"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", session.ID, session.Status, agentID, session.Directory, session.Title)
	}
	return nil
}

func call(method string, params any, destination any) error {
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request := protocol.Request{
		Version: protocol.Version, RequestID: fmt.Sprintf("cli-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Method: method, Params: encodedParams,
	}
	requestData, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	if err := server.CallLocal(base64.StdEncoding.EncodeToString(requestData), &output); err != nil {
		return err
	}
	var response struct {
		OK     bool               `json:"ok"`
		Result json.RawMessage    `json:"result"`
		Error  *protocol.RPCError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		return errors.New("falknd returned malformed JSON")
	}
	if !response.OK {
		if response.Error != nil {
			return errors.New(response.Error.Message)
		}
		return errors.New("falknd rejected the request")
	}
	return json.Unmarshal(response.Result, destination)
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func printUsage() {
	fmt.Println("falkn - persistent shells for coding agents")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  falkn                    Create or attach in the current directory")
	fmt.Println("  falkn new [name]         Create another persistent shell")
	fmt.Println("  falkn attach <id>        Attach to a session")
	fmt.Println("  falkn list               List sessions")
	fmt.Println("  falkn notifications      List the devices this machine notifies")
	fmt.Println("  falkn daemon status      Show client and daemon versions")
	fmt.Println("  falkn daemon restart     Safely load an installed daemon update")
	fmt.Println("  falkn version            Print version information")
	fmt.Println()

	prefix, err := server.AttachPrefix()
	if err != nil {
		// An unusable FALKN_PREFIX must not hide the rest of the help text;
		// document the default and let attaching report the real problem.
		prefix = server.DefaultAttachPrefix
	}
	name := server.AttachPrefixName(prefix)
	fmt.Println("Hotkeys, while attached to a session:")
	fmt.Printf("  %-25s%s\n", name+" then Q", "Detach; the session keeps running")
	fmt.Printf("  %-25s%s\n", name+" then N", "Create and switch to a new persistent session")
	fmt.Printf("  %-25s%s\n", name+" then M", "Mute or unmute notifications")
	fmt.Printf("  %-25s%s\n", name+" then Up", "Open the session switcher")
	fmt.Printf("  %-25s%s\n", name+" then Left/Right", "Switch to the previous or next session")
	fmt.Printf("  %-25s%s\n", name+" then 1-9", "Switch to a session by number")
	fmt.Printf("  %-25s%s\n", name+" twice", "Send a literal "+name+" to the session")
	fmt.Println()
	fmt.Println("Environment:")
	fmt.Printf(
		"  %-25sHotkey prefix, for example ctrl-a (default %s)\n",
		server.AttachPrefixVariable,
		strings.ToLower(server.AttachPrefixName(server.DefaultAttachPrefix)),
	)
}
