package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAskHumanHandler exercises the ask_human tool handler (§6.13): a real answer is returned as
// text; a no-answer sentinel, an unreachable bridge, and any error all map to the fallback
// sentinel; and the optional context is folded into the question.
func TestAskHumanHandler(t *testing.T) {
	answered := askHumanHandler(func(_ string, _ []string) (string, bool, error) { return "postgres", false, nil })
	res, _, err := answered(context.Background(), nil, askHumanInput{Question: "db?", Choices: []string{"postgres", "sqlite"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(res); got != "postgres" {
		t.Errorf("answer text = %q, want postgres", got)
	}

	for name, fn := range map[string]askVia{
		"no-answer":   func(_ string, _ []string) (string, bool, error) { return "", true, nil },
		"unreachable": func(_ string, _ []string) (string, bool, error) { return "", false, errors.New("dial fail") },
	} {
		res, _, _ := askHumanHandler(fn)(context.Background(), nil, askHumanInput{Question: "x"})
		if !strings.Contains(toolText(res), "NO_HUMAN_ANSWER") {
			t.Errorf("%s: expected sentinel, got %q", name, toolText(res))
		}
	}

	// Context is folded into the question the human sees.
	var gotQ string
	withCtx := askHumanHandler(func(q string, _ []string) (string, bool, error) { gotQ = q; return "ok", false, nil })
	if _, _, err := withCtx(context.Background(), nil, askHumanInput{Question: "why?", Context: "because reasons"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQ, "why?") || !strings.Contains(gotQ, "because reasons") {
		t.Errorf("context not folded into question: %q", gotQ)
	}
}

func toolText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}
