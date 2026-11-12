// SPDX-License-Identifier: GPL-3.0-or-later

// Command psuctl runs a server's admin commands through psud's loopback admin API
// (internal/admin). In Docker: docker compose exec psud psuctl <command>.
//
//	psuctl status | who
//	psuctl say [-scope all|uni:N|area:Q/Z|player:NAME] [-kind ticker|right|dialog|center] [-secs N] <text>
//	psuctl kick <name> [reason] | mute <name> [minutes] [reason] | unmute <name>
//	psuctl season [<name> <season|none|off>]
//	psuctl accounts [pattern] | create <account> | reset <account> | ban <account> [hours] [reason] | unban <account>
//	psuctl rename <account> <new> | delete <account> -yes [-force] | audit [n]
//	psuctl news | news add <text> | news rm <id>
//	psuctl events | events set <file.json | -> | events rm <key>
//	psuctl help [all] | help show <id> | help reply [-close] <id> <text> | help close <id>
//
// The token is $PSU_ADMIN_TOKEN, else $PSU_STATE_DIR/admin.token (psud writes it).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type client struct {
	base, token, by string
	http            *http.Client
}

func main() {
	fs := flag.NewFlagSet("psuctl", flag.ExitOnError)
	addr := fs.String("addr", env("PSU_ADMIN_ADDR", "127.0.0.1:22190"), "psud's admin API")
	stateDir := fs.String("state", env("PSU_STATE_DIR", "state"), "psud's state directory (for admin.token)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: psuctl [-addr host:port] [-state dir] <command> ...  (see the package comment, or psuctl help-commands)")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	if fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}
	token := strings.TrimSpace(os.Getenv("PSU_ADMIN_TOKEN"))
	if token == "" {
		b, err := os.ReadFile(filepath.Join(*stateDir, "admin.token"))
		if err != nil {
			fail(fmt.Errorf("no PSU_ADMIN_TOKEN and no token file: %w", err))
		}
		token = strings.TrimSpace(string(b))
	}
	by := env("SUDO_USER", env("USER", "psuctl"))
	c := &client{"http://" + *addr, token, by, &http.Client{Timeout: 15 * time.Second}}
	if err := c.run(fs.Arg(0), fs.Args()[1:]); err != nil {
		fail(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "psuctl:", err)
	os.Exit(1)
}

var errUsage = errors.New("usage: see psuctl help-commands")

const commands = `status | who
say [-scope all|uni:N|area:Q/Z|player:NAME] [-kind ticker|right|dialog|center] [-secs N] <text>
kick <name> [reason] | mute <name> [minutes] [reason] | unmute <name>
season [<name> <season|none|off>]
accounts [pattern] | create <account> | reset <account> | ban <account> [hours] [reason] | unban <account>
rename <account> <new> | delete <account> -yes [-force] | audit [n]
news | news add <text> | news rm <id>
events | events set <file.json | -> | events rm <key>
help [all] | help show <id> | help reply [-close] <id> <text> | help close <id>`

// call sends a request; out (when non-nil) receives the JSON reply.
func (c *client) call(method, path string, in, out any) error {
	var body io.Reader
	switch v := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(v)
	default:
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-PSU-By", c.by)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("is psud running? %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.Unmarshal(raw, &e)
		return fmt.Errorf("%s (%d)", strings.TrimSpace(cmp(e.Error, string(raw))), resp.StatusCode)
	}
	if out == nil {
		return printReply(raw)
	}
	return json.Unmarshal(raw, out)
}

func cmp(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// printReply prints a {"message"} reply as its text, anything else as indented JSON.
func printReply(raw []byte) error {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil && len(m) == 1 && m["message"] != nil {
		fmt.Println(m["message"])
		return nil
	}
	var buf bytes.Buffer
	json.Indent(&buf, raw, "", "  ")
	fmt.Println(buf.String())
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

func table(header string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, header)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

func when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (c *client) run(cmd string, args []string) error {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	rest := func(i int) string {
		if i < len(args) {
			return strings.Join(args[i:], " ")
		}
		return ""
	}
	switch cmd {
	case "help-commands":
		fmt.Println(commands)
		return nil
	case "status":
		return c.call("GET", "/v1/status", nil, nil)
	case "who":
		var rows []struct {
			Name, Account, GID, Game string
			Uni                      uint32
			Area                     []uint32
			Party, Muted             bool
		}
		if err := c.call("GET", "/v1/players", nil, &rows); err != nil {
			return err
		}
		var out [][]string
		for _, r := range rows {
			area := "-"
			if len(r.Area) == 3 {
				area = fmt.Sprintf("%d/%d/%d", r.Area[0], r.Area[1], r.Area[2])
			}
			out = append(out, []string{r.Name, r.Account, r.GID, r.Game, strconv.Itoa(int(r.Uni)), area,
				flags(r.Party, "party", r.Muted, "muted")})
		}
		fmt.Printf("%d online\n", len(rows))
		table("CHARACTER\tACCOUNT\tGID\tDISC\tUNI\tAREA\t", out)
		return nil
	case "say":
		fs := flag.NewFlagSet("say", flag.ContinueOnError)
		scope := fs.String("scope", "all", "all, uni:N, area:QUEST/ZONE or player:NAME")
		kind := fs.String("kind", "ticker", "ticker, right, dialog or center")
		secs := fs.Int("secs", 0, "how long a dialog stays (default 10)")
		if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
			return errUsage
		}
		return c.call("POST", "/v1/announce", map[string]any{"text": strings.Join(fs.Args(), " "), "scope": *scope,
			"kind": *kind, "seconds": *secs}, nil)
	case "kick":
		if arg(0) == "" {
			return errUsage
		}
		return c.call("POST", "/v1/players/"+esc(arg(0))+"/kick", map[string]string{"reason": rest(1)}, nil)
	case "mute":
		if arg(0) == "" {
			return errUsage
		}
		in := map[string]any{"minutes": 60, "reason": rest(1)}
		if n, err := strconv.Atoi(arg(1)); err == nil {
			in["minutes"], in["reason"] = n, rest(2)
		}
		return c.call("POST", "/v1/players/"+esc(arg(0))+"/mute", in, nil)
	case "unmute":
		if arg(0) == "" {
			return errUsage
		}
		return c.call("POST", "/v1/players/"+esc(arg(0))+"/unmute", nil, nil)
	case "season":
		if arg(0) == "" {
			return c.call("GET", "/v1/season", nil, nil)
		}
		if arg(1) == "" {
			return errUsage
		}
		return c.call("POST", "/v1/players/"+esc(arg(0))+"/season", map[string]string{"season": arg(1)}, nil)
	case "accounts":
		var rows []struct {
			GID                uint32
			Username           string
			ResetPending       bool
			Created, LastLogin time.Time
			Characters         []string
			BannedUntil        time.Time
			BanReason          string
		}
		if err := c.call("GET", "/v1/accounts?limit=200&q="+url.QueryEscape(arg(0)), nil, &rows); err != nil {
			return err
		}
		var out [][]string
		for _, r := range rows {
			ban := ""
			if r.BannedUntil.After(time.Now()) {
				ban = "banned until " + when(r.BannedUntil) + " " + r.BanReason
			}
			out = append(out, []string{r.Username, fmt.Sprintf("%#08x", r.GID), strings.Join(r.Characters, ", "),
				when(r.LastLogin), strings.TrimSpace(flags(r.ResetPending, "reset pending", false, "") + " " + ban)})
		}
		table("ACCOUNT\tGID\tCHARACTERS\tLAST LOGIN\t", out)
		return nil
	case "reset", "create":
		if arg(0) == "" {
			return errUsage
		}
		var r struct{ Account, Code, How string }
		method, path, body := "POST", "/v1/accounts/"+esc(arg(0))+"/reset", any(nil)
		if cmd == "create" {
			path, body = "/v1/accounts", map[string]string{"name": arg(0)}
		}
		if err := c.call(method, path, body, &r); err != nil {
			return err
		}
		fmt.Printf("%s: reset code %s\n(%s; it is shown only now)\n", r.Account, r.Code, r.How)
		return nil
	case "ban":
		if arg(0) == "" {
			return errUsage
		}
		in := map[string]any{"hours": 24.0, "reason": rest(1)}
		if h, err := strconv.ParseFloat(arg(1), 64); err == nil {
			in["hours"], in["reason"] = h, rest(2)
		}
		return c.call("POST", "/v1/accounts/"+esc(arg(0))+"/ban", in, nil)
	case "unban":
		if arg(0) == "" {
			return errUsage
		}
		return c.call("POST", "/v1/accounts/"+esc(arg(0))+"/unban", nil, nil)
	case "rename":
		if arg(1) == "" {
			return errUsage
		}
		return c.call("POST", "/v1/accounts/"+esc(arg(0))+"/rename", map[string]string{"to": arg(1)}, nil)
	case "delete":
		fs := flag.NewFlagSet("delete", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "really delete the account and everything under it")
		force := fs.Bool("force", false, "kick it first if it is online")
		if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
			return errUsage
		}
		if !*yes {
			return errors.New("deleting " + fs.Arg(0) + " removes its characters, cards, mail and requests for good: add -yes (back up first: make backup)")
		}
		q := ""
		if *force {
			q = "?force=1"
		}
		return c.call("DELETE", "/v1/accounts/"+esc(fs.Arg(0))+q, nil, nil)
	case "audit":
		n := cmp(arg(0), "20")
		var rows []struct {
			At               time.Time
			Op, Username, By string
			GID              uint32
		}
		if err := c.call("GET", "/v1/audit?n="+url.QueryEscape(n), nil, &rows); err != nil {
			return err
		}
		var out [][]string
		for _, r := range rows {
			out = append(out, []string{when(r.At), r.Op, r.Username, fmt.Sprintf("%#08x", r.GID), r.By})
		}
		table("WHEN\tOP\tACCOUNT\tGID\tBY", out)
		return nil
	case "news":
		switch arg(0) {
		case "":
			var rows []struct {
				ID       int64
				Text, By string
				Created  time.Time
			}
			if err := c.call("GET", "/v1/news", nil, &rows); err != nil {
				return err
			}
			for _, r := range rows {
				fmt.Printf("#%d  %s  by %s\n    %s\n", r.ID, when(r.Created), cmp(r.By, "-"),
					strings.ReplaceAll(r.Text, "\n", "\n    "))
			}
			return nil
		case "add":
			if rest(1) == "" {
				return errUsage
			}
			return c.call("POST", "/v1/news", map[string]string{"text": rest(1)}, nil)
		case "rm":
			return c.call("DELETE", "/v1/news/"+esc(arg(1)), nil, nil)
		}
	case "events":
		switch arg(0) {
		case "":
			return c.call("GET", "/v1/events", nil, nil)
		case "set":
			var spec []byte
			var err error
			if arg(1) == "-" {
				spec, err = io.ReadAll(os.Stdin)
			} else if arg(1) != "" {
				spec, err = os.ReadFile(arg(1))
			} else {
				return errUsage
			}
			if err != nil {
				return err
			}
			var head struct{ Key string }
			if err := json.Unmarshal(spec, &head); err != nil || head.Key == "" {
				return fmt.Errorf("the spec needs a key: %v", err)
			}
			return c.call("PUT", "/v1/events/"+esc(head.Key), spec, nil)
		case "rm":
			return c.call("DELETE", "/v1/events/"+esc(arg(1)), nil, nil)
		}
	case "help":
		switch arg(0) {
		case "", "all":
			var rows []struct {
				ID              int64
				Name, Subject   string
				Updated         time.Time
				Closed, Waiting bool
				Messages        int
			}
			q := ""
			if arg(0) == "all" {
				q = "?all=1"
			}
			if err := c.call("GET", "/v1/help"+q, nil, &rows); err != nil {
				return err
			}
			var out [][]string
			for _, r := range rows {
				out = append(out, []string{"#" + strconv.FormatInt(r.ID, 10), r.Name, r.Subject, when(r.Updated),
					strconv.Itoa(r.Messages), flags(r.Waiting, "waiting", r.Closed, "closed")})
			}
			table("REQUEST\tCHARACTER\tSUBJECT\tUPDATED\tMESSAGES\t", out)
			return nil
		case "show":
			var r struct {
				Thread struct {
					ID            int64
					Name, Subject string
					Closed        bool
				}
				Messages []struct {
					Staff     bool
					Text, By  string
					At        time.Time
					Delivered bool
				}
			}
			if err := c.call("GET", "/v1/help/"+esc(arg(1)), nil, &r); err != nil {
				return err
			}
			fmt.Printf("#%d %s: %s%s\n", r.Thread.ID, r.Thread.Name, r.Thread.Subject, flags(r.Thread.Closed, " (closed)", false, ""))
			for _, m := range r.Messages {
				who := r.Thread.Name
				if m.Staff {
					who = "staff " + m.By + flags(!m.Delivered, " (not shown yet)", false, "")
				}
				fmt.Printf("  %s  %s: %s\n", when(m.At), who, m.Text)
			}
			return nil
		case "reply":
			fs := flag.NewFlagSet("reply", flag.ContinueOnError)
			closeIt := fs.Bool("close", false, "close the request with the answer")
			if err := fs.Parse(args[1:]); err != nil || fs.NArg() < 2 {
				return errUsage
			}
			return c.call("POST", "/v1/help/"+esc(fs.Arg(0))+"/reply",
				map[string]any{"text": strings.Join(fs.Args()[1:], " "), "close": *closeIt}, nil)
		case "close":
			return c.call("POST", "/v1/help/"+esc(arg(1))+"/close", nil, nil)
		}
	}
	return errUsage
}

func flags(a bool, an string, b bool, bn string) string {
	var out []string
	if a {
		out = append(out, an)
	}
	if b {
		out = append(out, bn)
	}
	return strings.Join(out, " ")
}
