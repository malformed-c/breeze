package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"breeze/internal/wire"
)

// cmdTask is create/list/update for work items — a unit of work with people
// attached to it, as distinct from a stage (which is something breeze RUNS) and
// from an hours task (which is where time WENT).
func cmdTask(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze create task | list tasks | update task ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)

	switch sub {
	case "create":
		if handled, err := f.only(`breeze create task "<title>" [--assign NAME] [--review NAME] [--json] --as WHO [--token T | --token-file PATH]`); handled {
			return err
		}
		if len(f.rest) < 1 {
			return fmt.Errorf(`usage: breeze create task "<title>" [--assign NAME] [--review NAME] --as WHO`)
		}
		as := resolveIdentity(p, f)
		token, _ := resolveTokenAuto(p, f, as)
		payload, _ := json.Marshal(wire.TaskCreateRequest{
			Title: strings.Join(f.rest, " "), Assignee: f.assign, Reviewer: f.review,
		})
		resp, err := call(p, wire.Request{Op: wire.OpTaskCreate, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.TaskResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		fmt.Printf("%s created: %s\n", out.Item.ID, out.Item.Title)
		printNotified(out.Notified)
		return nil

	case "list":
		if handled, err := f.only("breeze list tasks [--status S] [--assign NAME] [--json]"); handled {
			return err
		}
		resp, err := call(p, wire.Request{Op: wire.OpTaskList})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.TaskListResponse](resp)
		if err != nil {
			return err
		}
		items := filterTasks(out.Items, f)
		if f.jsonOut {
			printJSON(wire.TaskListResponse{Items: items})
			return nil
		}
		printTasks(items)
		return nil

	case "update":
		if handled, err := f.only(`breeze update task <id> [--status S] [--assign NAME] [--review NAME] [--blocked "why"] [--json] --as WHO [--token T | --token-file PATH]`); handled {
			return err
		}
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze update task <id> [--status S] [--assign NAME] [--review NAME] [--blocked \"why\"] --as WHO")
		}
		as := resolveIdentity(p, f)
		token, _ := resolveTokenAuto(p, f, as)
		req := wire.TaskUpdateRequest{ID: f.rest[0]}
		// Only fields the caller actually NAMED are sent: a pointer left nil means
		// "leave alone", and --assign "" means unassign. Sending everything would
		// make every update silently overwrite fields nobody mentioned.
		if f.seen["--status"] {
			req.Status = &f.status
		}
		if f.seen["--assign"] {
			req.Assignee = &f.assign
		}
		if f.seen["--review"] {
			req.Reviewer = &f.review
		}
		if f.seen["--blocked"] {
			req.Blocked = &f.blocked
		}
		payload, _ := json.Marshal(req)
		resp, err := call(p, wire.Request{Op: wire.OpTaskUpdate, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.TaskResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		fmt.Printf("%s [%s] %s\n", out.Item.ID, out.Item.Status, out.Item.Title)
		printNotified(out.Notified)
		return nil
	}
	return fmt.Errorf("unknown task subcommand %q (create, list, update)", sub)
}

// printNotified says who heard about it, including when that is nobody —
// "notified: (nobody)" is a real answer, and silence would let a caller assume
// the change reached someone it did not.
func printNotified(who []string) {
	if len(who) == 0 {
		fmt.Println("notified: (nobody — no other identity is attached to this task)")
		return
	}
	fmt.Printf("notified: %s\n", strings.Join(who, ", "))
}

func filterTasks(items []wire.WorkItem, f flagSet) []wire.WorkItem {
	if f.status == "" && f.assign == "" {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if f.status != "" && it.Status != f.status {
			continue
		}
		if f.assign != "" && it.Assignee != f.assign {
			continue
		}
		out = append(out, it)
	}
	return out
}

func printTasks(items []wire.WorkItem) {
	if len(items) == 0 {
		fmt.Println("no tasks — `breeze create task \"...\"` opens one")
		return
	}
	idW, titleW := len("ID"), len("TITLE")
	for _, it := range items {
		idW, titleW = max(idW, len(it.ID)), max(titleW, len(it.Title))
	}
	if titleW > 52 {
		titleW = 52
	}
	fmt.Printf("%-*s  %-8s  %-*s  %-12s  %-12s  %s\n", idW, "ID", "STATUS", titleW, "TITLE", "ASSIGNEE", "REVIEWER", "CREATOR")
	for _, it := range items {
		title := it.Title
		if len(title) > titleW {
			title = title[:titleW-1] + "…"
		}
		line := fmt.Sprintf("%-*s  %-8s  %-*s  %-12s  %-12s  %s",
			idW, it.ID, it.Status, titleW, title, orDash(it.Assignee), orDash(it.Reviewer), orDash(it.Creator))
		fmt.Println(strings.TrimRight(line, " "))
		// A blocked item that does not say what it waits for is a status nobody can
		// act on, so the reason is printed rather than hidden behind --json.
		if it.Status == "blocked" && it.Blocked != "" {
			fmt.Printf("%-*s  └─ blocked: %s\n", idW, "", it.Blocked)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
