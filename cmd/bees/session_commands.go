package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/issues"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// mailStateDir decides which state directory "bees mail" talks to, in order:
// an explicit -c/--config (the flag has no default, so a non-empty value was
// typed), then $BEES_STATE_DIR (how a session reaches its own mailbox without
// searching for a bees.toml), then the config found by configPath. load is only
// called in the first and last case.
func mailStateDir(explicitConfig, envStateDir string, load func() (*config.Config, error)) (string, error) {
	if explicitConfig == "" && envStateDir != "" {
		return envStateDir, nil
	}
	cfg, err := load()
	if err != nil {
		return "", err
	}
	return cfg.StateDir(), nil
}

// mailbox returns the mailbox for the current context and the state directory
// it lives in.
func mailbox(g *globalFlags) (*mail.Box, string, error) {
	dir, err := mailStateDir(g.config, os.Getenv(session.EnvStateDir), func() (*config.Config, error) {
		return loadConfig(g)
	})
	if err != nil {
		return nil, "", err
	}
	return mail.Open(state.New(dir).MailDir()), dir, nil
}

// ---- mail ------------------------------------------------------------------

func newMailCmd(g *globalFlags) *cobra.Command {
	cmd := groupCmd("mail", "Send and read messages in the local mailbox")
	cmd.Long = `The mailbox is how roles talk to each other. Sessions send mail with
"bees mail send"; the orchestrator delivers it by including it in the prompt of
the session working on the referenced issue or PR. Humans can read and send
mail too (use --from human).`

	var to, from, subject, body, bodyFile, replyTo string
	var issue, pr int
	send := &cobra.Command{
		Use:   "send",
		Short: "Send a message to a role",
		RunE: func(cmd *cobra.Command, args []string) error {
			box, dir, err := mailbox(g)
			if err != nil {
				return err
			}
			toRole, err := config.CanonicalRole(to)
			if err != nil {
				return err
			}
			if from == "" {
				from = os.Getenv(session.EnvRole)
			}
			if from == "" {
				return errors.New("--from is required outside a session (e.g. --from human)")
			}
			if issue == 0 {
				issue = envInt(session.EnvIssue)
			}
			if pr == 0 {
				pr = envInt(session.EnvPR)
			}
			text, err := readBody(body, bodyFile)
			if err != nil {
				return err
			}
			m, err := box.Send(mail.Message{To: toRole, From: from, Subject: subject, Body: text, Issue: issue, PR: pr, InReplyTo: replyTo})
			if err != nil {
				return err
			}
			fmt.Printf("sent %s to %s (%s)\n", m.ID, m.To, dir)
			return nil
		},
	}
	send.Flags().StringVar(&to, "to", "", "recipient role (required)")
	send.Flags().StringVar(&from, "from", "", "sender (default: $BEES_ROLE)")
	send.Flags().StringVar(&subject, "subject", "", "subject line")
	send.Flags().StringVar(&body, "body", "", "message body")
	send.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file (\"-\" for stdin)")
	send.Flags().StringVar(&replyTo, "in-reply-to", "", "id of the message being answered")
	send.Flags().IntVar(&issue, "issue", 0, "issue number the message is about (default: $BEES_ISSUE)")
	send.Flags().IntVar(&pr, "pr", 0, "pull request number the message is about (default: $BEES_PR)")
	_ = send.MarkFlagRequired("to")

	var lTo, lFrom string
	var lIssue, lPR int
	var unread, full bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			box, _, err := mailbox(g)
			if err != nil {
				return err
			}
			if lTo != "" {
				if lTo, err = config.CanonicalRole(lTo); err != nil {
					return err
				}
			}
			msgs, err := box.List(mail.Filter{To: lTo, From: lFrom, Issue: lIssue, PR: lPR, UnreadOnly: unread})
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				fmt.Println("no messages")
				return nil
			}
			for _, m := range msgs {
				if full {
					fmt.Println(mail.Format(m))
					continue
				}
				flag := " "
				if m.Unread() {
					flag = "*"
				}
				ctx := ""
				if m.Issue > 0 {
					ctx += fmt.Sprintf(" issue#%d", m.Issue)
				}
				if m.PR > 0 {
					ctx += fmt.Sprintf(" pr#%d", m.PR)
				}
				fmt.Printf("%s %s  %-16s -> %-16s%s  %s\n", flag, m.ID, m.From, m.To, ctx, m.Subject)
			}
			return nil
		},
	}
	list.Flags().StringVar(&lTo, "to", "", "recipient role")
	list.Flags().StringVar(&lFrom, "from", "", "sender")
	list.Flags().IntVar(&lIssue, "issue", 0, "issue number")
	list.Flags().IntVar(&lPR, "pr", 0, "pull request number")
	list.Flags().BoolVar(&unread, "unread", false, "only unread messages")
	list.Flags().BoolVar(&full, "full", false, "print full messages")

	read := &cobra.Command{
		Use:   "read <id>",
		Short: "Print one message",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			box, _, err := mailbox(g)
			if err != nil {
				return err
			}
			m, err := box.Get(args[0])
			if err != nil {
				return err
			}
			fmt.Print(mail.Format(m))
			return nil
		},
	}

	cmd.AddCommand(send, list, read)
	return cmd
}

// ---- issue -----------------------------------------------------------------

func newIssueCmd(g *globalFlags) *cobra.Command {
	cmd := groupCmd("issue", "Create factory issues (visible, labelled, attached to a parent, in the right milestone)")
	cmd.Long = `issue create makes an issue the way the factory wants it: with the
visibility label and assignee, the kind label (--bug, --feature) and the
state label (triage, or --ready), attached as a GitHub sub-issue of --parent,
and in the milestone of the --parent or --related issue. Milestones are
managed by people; the factory only inherits them.`
	var title, body, bodyFile, milestone string
	var parent, related int
	var blockedBy []int
	var bug, feature, ready bool
	var extra []string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an issue",
		Example: `  bees issue create --parent 12 --title "Export as CSV" --body-file body.md   # work item, child of feature #12
  bees issue create --bug --related $BEES_ISSUE --title "Crash on empty input" --body "..."
  bees issue create --feature --related 40 --title "Search" --body-file body.md   # feature from feedback #40
  bees issue create --parent 12 --blocked-by 37 --title "Order the queue" --body-file body.md  # not built before #37 closes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			text, err := readBody(body, bodyFile)
			if err != nil {
				return err
			}
			kind := issues.KindTask
			switch {
			case bug && feature:
				return errors.New("--bug and --feature are exclusive")
			case bug:
				kind = issues.KindBug
			case feature:
				kind = issues.KindFeature
			}
			res, err := issues.Create(cmd.Context(), a.gh, a.cfg.Filter, a.cfg.Labels(), issues.Options{
				Title: title, Body: text, Kind: kind, Parent: parent, Related: related, Milestone: milestone, ExtraLabels: extra, Ready: ready, BlockedBy: blockedBy,
			})
			if err != nil {
				return err
			}
			fmt.Println(res)
			return nil
		},
	}
	create.Flags().StringVar(&title, "title", "", "issue title (required)")
	create.Flags().StringVar(&body, "body", "", "issue body")
	create.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file (\"-\" for stdin)")
	create.Flags().IntVar(&parent, "parent", 0, "make it a sub-issue of this feature issue (inherits its milestone)")
	create.Flags().IntVar(&related, "related", 0, "inherit the milestone of this issue without attaching")
	create.Flags().StringVar(&milestone, "milestone", "", "milestone title (overrides inheritance)")
	create.Flags().BoolVar(&bug, "bug", false, "bug work item")
	create.Flags().BoolVar(&feature, "feature", false, "feature issue (owned by the product manager, no state label)")
	create.Flags().BoolVar(&ready, "ready", false, "work item is already detailed: skip triage")
	create.Flags().IntSliceVar(&blockedBy, "blocked-by", nil, "issue this one must not be built before (repeatable); written as a \"Blocked by #N\" line the scheduler honours")
	create.Flags().StringArrayVar(&extra, "label", nil, "extra label (repeatable)")
	_ = create.MarkFlagRequired("title")

	var lParent, lChild int
	link := &cobra.Command{
		Use:   "link",
		Short: "Attach an existing issue as a sub-issue of a feature issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			res, err := issues.Link(cmd.Context(), a.gh, a.cfg.Labels(), lParent, lChild)
			if err != nil {
				return err
			}
			fmt.Println(res)
			return nil
		},
	}
	link.Flags().IntVar(&lParent, "parent", 0, "feature issue number (required)")
	link.Flags().IntVar(&lChild, "child", 0, "issue to attach (required)")
	_ = link.MarkFlagRequired("parent")
	_ = link.MarkFlagRequired("child")

	cmd.AddCommand(create, link)
	return cmd
}

// ---- done ------------------------------------------------------------------

// doneLong builds the done command's Long text from the same authority
// (session.ValidOutcomes) that validates a reported outcome, so the two
// cannot drift apart. The role column is aligned to the longest role name
// in config.Roles.
func doneLong() string {
	width := 0
	for _, role := range config.Roles {
		if len(role) > width {
			width = len(role)
		}
	}
	var b strings.Builder
	b.WriteString("done records the session's outcome for the orchestrator. Valid statuses:\n\n")
	for _, role := range config.Roles {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, role, strings.Join(session.ValidOutcomes(role), ", "))
	}
	b.WriteString("\npr-opened and pr-updated require a pull request number (--pr or $BEES_PR).")
	return b.String()
}

func newDoneCmd() *cobra.Command {
	var note string
	var pr, issue int
	cmd := &cobra.Command{
		Use:   "done <status>",
		Short: "Report the outcome of the current session (run by sessions, last thing they do)",
		Long:  doneLong(),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pr == 0 {
				pr = envInt(session.EnvPR)
			}
			if issue == 0 {
				issue = envInt(session.EnvIssue)
			}
			o, err := session.Report(os.Getenv(session.EnvSessionDir), os.Getenv(session.EnvRole),
				session.Outcome{Status: args[0], Note: note, PR: pr, Issue: issue})
			if err != nil {
				return err
			}
			fmt.Printf("outcome recorded: %s\n", o.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&note, "message", "m", "", "short note for the orchestrator and the logs")
	cmd.Flags().IntVar(&pr, "pr", 0, "pull request number (default: $BEES_PR)")
	cmd.Flags().IntVar(&issue, "issue", 0, "issue number (default: $BEES_ISSUE)")
	return cmd
}

func envInt(name string) int {
	var n int
	_, _ = fmt.Sscanf(os.Getenv(name), "%d", &n)
	return n
}
