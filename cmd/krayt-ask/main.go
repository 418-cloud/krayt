// Command krayt-ask is the universal in-container front-end of the agent → human question
// channel (§6.13). Any agent can shell out to it — `krayt-ask [--choices a,b] "question"` — and
// it prints the human's answer on stdout (exit 0), or, when there is no human (fail mode, a
// timeout, or the bridge is unreachable), exits non-zero with an empty stdout so the agent
// falls back gracefully. It connects to the in-VM ask bridge over the mounted unix socket
// (default /run/krayt/ask.sock, overridable via KRAYT_ASK_SOCKET). This is the
// lowest-common-denominator front-end; the MCP server is the premium path (Phase 6).
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/418-cloud/krayt/internal/guest/ask"
)

// defaultSocket is the container-side ask-bridge socket path (§6.13, §8.2).
const defaultSocket = "/run/krayt/ask.sock"

// Exit codes: answered cleanly, no-answer sentinel (agent should fall back), and usage error.
const (
	exitAnswered = 0
	exitNoAnswer = 2
	exitUsage    = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv("KRAYT_ASK_SOCKET"), os.Stdout, os.Stderr))
}

// run is the testable core: it parses args, submits the question over the socket, and maps the
// outcome to an exit code. socketEnv is the KRAYT_ASK_SOCKET value ("" → default path).
func run(args []string, socketEnv string, stdout, stderr io.Writer) int {
	var choices string
	var prompt []string
	// Hand-rolled flag scan (no flag package) so a `--` or a question that starts with text is
	// unambiguous and unknown leading flags are a usage error, not a silent no-answer.
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i++
			prompt = append(prompt, args[i:]...)
			i = len(args)
		case a == "--choices" || a == "-choices":
			if i+1 >= len(args) {
				_, _ = fmt.Fprintln(stderr, "krayt-ask: --choices needs a value")
				return exitUsage
			}
			i++
			choices = args[i]
		case strings.HasPrefix(a, "--choices="):
			choices = strings.TrimPrefix(a, "--choices=")
		case strings.HasPrefix(a, "-"):
			_, _ = fmt.Fprintf(stderr, "krayt-ask: unknown flag %q\n", a)
			return exitUsage
		default:
			prompt = append(prompt, args[i:]...)
			i = len(args)
		}
	}

	question := strings.TrimSpace(strings.Join(prompt, " "))
	if question == "" {
		_, _ = fmt.Fprintln(stderr, "usage: krayt-ask [--choices a,b,c] \"question\"")
		return exitUsage
	}

	socket := socketEnv
	if socket == "" {
		socket = defaultSocket
	}
	var ch []string
	for _, c := range strings.Split(choices, ",") {
		if c = strings.TrimSpace(c); c != "" {
			ch = append(ch, c)
		}
	}

	resp, noAnswer, err := ask.OverSocket(socket, question, ch)
	if err != nil {
		// Bridge unreachable (fail mode / not wired) → sentinel so the agent proceeds (§6.13).
		_, _ = fmt.Fprintf(stderr, "krayt-ask: no answer (%v)\n", err)
		return exitNoAnswer
	}
	if noAnswer {
		_, _ = fmt.Fprintln(stderr, "krayt-ask: no answer (declined or timed out)")
		return exitNoAnswer
	}
	_, _ = fmt.Fprintln(stdout, resp)
	return exitAnswered
}
