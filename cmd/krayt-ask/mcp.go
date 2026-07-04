package main

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/418-cloud/krayt/internal/guest/ask"
)

// askHumanInput is the ask_human tool's argument schema (§6.13); the field docs become the JSON
// schema descriptions the agent sees.
type askHumanInput struct {
	Question string   `json:"question" jsonschema:"the question to ask the human; ask ONLY when genuinely blocked on a decision a human must make"`
	Choices  []string `json:"choices,omitempty" jsonschema:"optional list of allowed answers to offer the human"`
	Context  string   `json:"context,omitempty" jsonschema:"optional extra background to help the human answer"`
}

// askVia submits a question to the in-VM bridge and returns the human's answer (or a no-answer
// sentinel). Injectable so the handler is testable without a socket.
type askVia func(question string, choices []string) (answer string, noAnswer bool, err error)

// runMCP serves the ask_human tool over stdio (§6.13). An MCP-speaking agent (Claude Code et al.)
// launches `krayt-ask --mcp` as a subprocess; a tools/call bridges to the question channel over
// the socket. This is the premium front-end; the bare `krayt-ask` CLI is the universal fallback.
func runMCP(socket string) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "krayt-ask-human", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "ask_human",
		Description: "Ask the human supervising this sandboxed run a question and wait for their answer. " +
			"Use ONLY when genuinely blocked on a decision a human must make — ambiguous requirements, a " +
			"risky or irreversible choice, or information you cannot obtain yourself. Do not use it for " +
			"anything you can decide, infer, or look up. If no human is available you'll get a sentinel " +
			"telling you to proceed autonomously.",
	}, askHumanHandler(func(q string, choices []string) (string, bool, error) {
		return ask.OverSocket(socket, q, choices)
	}))
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// askHumanHandler builds the tool handler around an askVia. On a real answer it returns the text;
// on no-answer (fail mode / timeout / unreachable bridge) it returns a clear sentinel so the agent
// falls back gracefully instead of stalling.
func askHumanHandler(fn askVia) func(context.Context, *mcp.CallToolRequest, askHumanInput) (*mcp.CallToolResult, any, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in askHumanInput) (*mcp.CallToolResult, any, error) {
		q := in.Question
		if strings.TrimSpace(in.Context) != "" {
			q = q + "\n\n" + in.Context
		}
		resp, noAnswer, err := fn(q, in.Choices)
		if err != nil || noAnswer {
			return textResult("NO_HUMAN_ANSWER — no human is available (fail mode, timeout, or bridge unreachable). Proceed autonomously with a reasonable default."), nil, nil
		}
		return textResult(resp), nil, nil
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
