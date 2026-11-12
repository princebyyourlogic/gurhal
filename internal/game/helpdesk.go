// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The help desk: "!gm <message>" in chat reaches the staff; their answer shows in game as
// a timed dialog "Staff: <text>", at once when the player is online, else after their next
// lobby load. One open request per account; a later !gm after a close opens a new one.

const (
	helpTextMax    = 400
	helpSubjectMax = 60
	helpWindow     = 10 * time.Minute
	helpPerWindow  = 5
	helpDialogSecs = 15
)

func clean(text string, n int) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	return string(r[:min(len(r), n)])
}

// helpAsk is a player's !gm: the line to show them.
func (s *Server) helpAsk(ctx context.Context, gid uint32, name, text string) string {
	if text = clean(text, helpTextMax); text == "" {
		return s.helpStatus(ctx, gid)
	}
	now := time.Now()
	if n, err := s.Store.HelpSent(ctx, gid, now.Add(-helpWindow)); err != nil || n >= helpPerWindow {
		return "You have sent the staff several messages already. Please wait a few minutes."
	}
	id, err := s.Store.HelpAsk(ctx, gid, name, clean(text, helpSubjectMax), text, now)
	if err != nil {
		s.Log.Error("help ask", "err", err)
		return "The help desk is not available right now."
	}
	s.Log.Info("help ask", "thread", id, "gid", fmt.Sprintf("%#08x", gid), "chars", len(text))
	return fmt.Sprintf("Sent to the staff (request #%d). Their answer will show up here.", id)
}

func (s *Server) helpStatus(ctx context.Context, gid uint32) string {
	id, ok, err := s.Store.HelpOpen(ctx, gid)
	if err != nil || !ok {
		return "To reach the staff, type !gm and your message."
	}
	answer, ok, _ := s.Store.HelpLatestAnswer(ctx, id)
	if !ok {
		return fmt.Sprintf("Request #%d is open; the staff have not answered yet.", id)
	}
	return fmt.Sprintf("Request #%d, latest answer: %s", id, answer)
}

// deliverHelp shows gid its undelivered staff answers if it is online.
func (s *Server) deliverHelp(gid uint32) int {
	p := s.world.find(gid)
	if p == nil {
		return 0
	}
	msgs, err := s.Store.HelpUndelivered(s.ctx(), gid)
	if err != nil {
		s.Log.Error("help deliver", "err", err)
		return 0
	}
	for _, m := range msgs {
		say(p, "Staff: "+m.Text, helpDialogSecs)
		s.Store.HelpDelivered(s.ctx(), m.ID)
	}
	return len(msgs)
}

// HelpReply is a staff answer (the admin API): shown now when the player is online.
func (s *Server) HelpReply(ctx context.Context, thread int64, text, by string, closeIt bool) (string, error) {
	if text = clean(text, helpTextMax); text == "" {
		return "", fmt.Errorf("the answer is empty")
	}
	gid, err := s.Store.HelpReply(ctx, thread, text, by, closeIt, time.Now())
	if err != nil {
		return "", err
	}
	s.init()
	where := "shown when they next enter a lobby"
	if s.deliverHelp(gid) > 0 {
		where = "shown in game now"
	}
	s.Log.Info("help reply", "thread", thread, "by", by, "closed", closeIt)
	if closeIt {
		return "Answered (" + where + ") and closed.", nil
	}
	return "Answered (" + where + ").", nil
}

// HelpClose closes a request (the admin API), telling the player if online.
func (s *Server) HelpClose(ctx context.Context, thread int64, by string) (string, error) {
	gid, was, err := s.Store.HelpClose(ctx, thread, time.Now())
	if err != nil || was {
		return "It was already closed.", err
	}
	s.init()
	if p := s.world.find(gid); p != nil {
		say(p, fmt.Sprintf("The staff closed your request #%d. Type !gm to send a new one.", thread), helpDialogSecs)
	}
	s.Log.Info("help close", "thread", thread, "by", by)
	return "Closed.", nil
}
