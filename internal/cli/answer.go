package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/controlclient"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/protocol/pb"
)

func newAnswerCmd() *cobra.Command {
	var repo string
	var noAnswer bool
	cmd := &cobra.Command{
		Use:   "answer <run-id> [<question-id>] <response>",
		Short: "Answer a waiting run's agent question (§6.13)",
		Long: "Delivers a human answer to an agent that paused with ask_human. Omit the " +
			"question-id to answer the newest pending question. Use --no-answer to send the " +
			"'no answer' sentinel so the agent falls back on its own.",
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			runID := args[0]
			runDir := orchestrator.RunDir(sd, runID)
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				return fmt.Errorf("no such run %q: %w", runID, err)
			}

			qid, response, err := resolveAnswerArgs(runDir, args[1:], noAnswer)
			if err != nil {
				return err
			}
			if rec.CtrlSocket == "" {
				return fmt.Errorf("run %q has no recorded control socket; cannot reach its guest", runID)
			}

			client, err := controlclient.DialSocket(rec.CtrlSocket)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			ack, err := client.Agent.Answer(cmd.Context(), &pb.AnswerRequest{
				QuestionId: qid, Response: response, NoAnswer: noAnswer,
			})
			if err != nil {
				return fmt.Errorf("deliver answer: %w", err)
			}
			if !ack.GetOk() {
				return fmt.Errorf("no pending question %q on run %q (already answered or timed out)", qid, runID)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "answered %s question %s\n", runID, qid)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	cmd.Flags().BoolVar(&noAnswer, "no-answer", false, "send the 'no answer' sentinel instead of a response")
	return cmd
}

// resolveAnswerArgs interprets the positional args after the run id:
//
//	answer <id> <response>            -> newest pending question, given response
//	answer <id> <qid> <response>      -> that question, given response
//	answer <id> --no-answer           -> newest pending question, sentinel (no response arg)
//	answer <id> <qid> --no-answer     -> that question, sentinel
func resolveAnswerArgs(runDir string, rest []string, noAnswer bool) (qid, response string, err error) {
	switch len(rest) {
	case 0:
		if !noAnswer {
			return "", "", fmt.Errorf("a <response> is required unless --no-answer is set")
		}
		qid, err = newestQuestionID(runDir)
		return qid, "", err
	case 1:
		if noAnswer {
			// the single arg is the question id; the sentinel carries no response
			return rest[0], "", nil
		}
		qid, err = newestQuestionID(runDir)
		return qid, rest[0], err
	default: // 2
		return rest[0], rest[1], nil
	}
}

// newestQuestionID returns the id of the most recently asked question for a run.
func newestQuestionID(runDir string) (string, error) {
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil {
		return "", err
	}
	if len(qs) == 0 {
		return "", fmt.Errorf("run has no recorded questions to answer")
	}
	return qs[len(qs)-1].ID, nil
}
