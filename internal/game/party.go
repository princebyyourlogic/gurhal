// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"

	"github.com/princebyyourlogic/gurhal/internal/rules"
)

// Parties (EGS social, PS2 layouts):
//
//	C>S 0x100A invite (+0x24 GID)        -> 0x100A to the invitee (or 0x100C 4: can't)
//	C>S 0x100C answer (+0x34 result 1)   -> 0x100C to the inviter; 1004 / 1005 on joining
//	C>S 0x1002 leave, 0x1007 kick (slot), 0x1013 new leader (slot), 0x101D invite setting
//	S>C 0x1006 events: 3 removed, 4 member gone, 7 new leader, 10 mission end, 12 cancelled
//	S>C 0x1022 a member's HP (+0x28 its slot)

func init() {
	handle(0x100A, 0x68, (*session).partyInvite)
	handle(0x100C, 0x78, (*session).partyAnswer)
	handle(0x1002, 0, (*session).partyLeaveCmd)
	handle(0x1007, 0x28, (*session).partyKick)
	handle(0x101D, 0x28, (*session).partyInviteSetting)
	handle(0x1013, 0x28, (*session).partyLeader)
}

// info is what a party window shows of p (takes p's character lock briefly).
func (p *player) info() memberInfo {
	m := memberInfo{gid: p.gid, disc: p.disc, level: 1, hp: 100, maxHP: 100}
	if w := p.where.Load(); w != nil {
		m.where = *w
	}
	if c := p.char.Load(); c != nil {
		c.mu.Lock()
		m.raw = c.raw
		m.maxHP = p.r.Derived(c.raw, c.st).MaxHP
		m.level, m.hp = c.st.Level, min(c.st.HP, m.maxHP)
		c.mu.Unlock()
	}
	return m
}

func (p *player) lid() uint16 {
	if w := p.where.Load(); w != nil {
		return w.lid
	}
	return 0
}

// partyHP sends p's HP to its party's HUDs (alone: to p, slot 0). Call without holding
// any character lock.
func (w *world) partyHP(p *player, hp int) {
	w.partyMu.Lock()
	members, k := []*player{p}, 0
	if pt := p.party; pt != nil {
		members, k = pt.members(), pt.slotOf(p)
	}
	w.partyMu.Unlock()
	for _, m := range members {
		m.conn.Relay(c1022HP(m.gid, m.lid(), hp, k))
	}
}

// partyOf is p's party (nil alone) and p's slot.
func (w *world) partyOf(p *player) (*party, int) {
	w.partyMu.Lock()
	defer w.partyMu.Unlock()
	if p.party == nil {
		return nil, 0
	}
	return p.party, p.party.slotOf(p)
}

// partyLocation sends p's location to the rest of its party (0x100E).
func (w *world) partyLocation(p *player) {
	w.partyMu.Lock()
	var others []*player
	k := 0
	if pt := p.party; pt != nil {
		k = pt.slotOf(p)
		for _, m := range pt.members() {
			if m != p {
				others = append(others, m)
			}
		}
	}
	w.partyMu.Unlock()
	if len(others) == 0 {
		return
	}
	info := p.info()
	for _, m := range others {
		m.conn.Relay(c100eMember(m.gid, m.lid(), k, info, m.disc))
	}
}

func (s *session) partyInvite(b []byte) error {
	if s.me == nil {
		return nil
	}
	target := binary.LittleEndian.Uint32(b[0x24:])
	other := s.srv.world.find(target)
	w := s.srv.world
	w.partyMu.Lock()
	busy := other == nil || (other.party != nil && len(other.party.members()) > 1)
	if !busy {
		w.pending[other.gid] = s.me
	}
	w.partyMu.Unlock()
	if busy {
		return s.c.Send(c100cError(s.gid, s.lid, target, 4, b[0x28:0x68]))
	}
	info := s.me.info()
	other.conn.Relay(c100aInvite(other.gid, other.lid(), s.gid, info.raw))
	return nil
}

func (s *session) partyAnswer(b []byte) error {
	if s.me == nil {
		return nil
	}
	w := s.srv.world
	w.partyMu.Lock()
	inviter := w.pending[s.gid]
	delete(w.pending, s.gid)
	w.partyMu.Unlock()
	if inviter == nil || w.find(inviter.gid) != inviter {
		return nil
	}
	inviter.conn.Relay(c100cResult(inviter.gid, inviter.lid(), b))
	if b[0x34] == 1 {
		s.partyJoin(inviter, s.me)
	}
	return nil
}

// partyJoin puts invitee into inviter's party (making one): 1004 to the members, 1005 to
// the newcomer.
func (s *session) partyJoin(inviter, invitee *player) {
	w := s.srv.world
	w.partyMu.Lock()
	pt := inviter.party
	if pt == nil {
		pt = &party{}
		pt.slots[0] = inviter
		inviter.party = pt
	}
	slot := pt.slotOf(nil)
	if pt.slotOf(invitee) >= 0 || slot < 0 {
		w.partyMu.Unlock()
		return
	}
	pt.slots[slot] = invitee
	invitee.party = pt
	members, leader := pt.members(), pt.leader
	slots := pt.slots
	w.partyMu.Unlock()

	infos := map[*player]memberInfo{}
	for _, m := range members {
		infos[m] = m.info()
	}
	for _, m := range members {
		if m != invitee {
			m.conn.Relay(c1004Member(m.gid, m.lid(), memberRecord(infos[invitee], slot, m.disc)))
		}
	}
	var recs [6][]byte
	for i, m := range slots {
		if m != nil {
			recs[i] = memberRecord(infos[m], i, invitee.disc)
		}
	}
	invitee.conn.Relay(c1005Party(invitee.gid, invitee.lid(), slot, leader, recs))
}

func (s *session) partyLeaveCmd(_ []byte) error {
	if s.me != nil {
		s.srv.world.partyLeave(s.me, false)
	}
	return nil
}

// partyLeave takes p out of its party (kicked: by the leader): 1006 to the others, a solo
// 1005 to p; a party of one dissolves.
func (w *world) partyLeave(p *player, kicked bool) {
	w.partyMu.Lock()
	pt := p.party
	if pt == nil {
		w.partyMu.Unlock()
		return
	}
	slot := pt.slotOf(p)
	pt.slots[slot] = nil
	p.party = nil
	rest := pt.members()
	newLeader := -1
	if slot == pt.leader && len(rest) > 0 {
		pt.leader = pt.slotOf(rest[0])
		newLeader = pt.leader
	}
	var last *player
	if len(rest) == 1 {
		last = rest[0]
		last.party = nil
	}
	w.partyMu.Unlock()

	online := w.find(p.gid) == p
	if online {
		if kicked {
			p.conn.Relay(c1006Party(p.gid, p.lid(), 3, uint8(slot)))
		}
		p.conn.Relay(c1005Solo(p.gid, p.lid(), p.info().raw))
	}
	ev := uint8(4)
	if kicked {
		ev = 13
	}
	for _, m := range rest {
		m.conn.Relay(c1006Party(m.gid, m.lid(), ev, uint8(slot)))
		if newLeader >= 0 {
			m.conn.Relay(c1006Party(m.gid, m.lid(), 7, uint8(newLeader)))
		}
	}
	if last != nil {
		last.conn.Relay(c1005Solo(last.gid, last.lid(), last.info().raw))
	}
}

func (s *session) partyKick(b []byte) error {
	if s.me == nil {
		return nil
	}
	w := s.srv.world
	w.partyMu.Lock()
	var victim *player
	if pt := s.me.party; pt != nil && b[0x24] < 6 && pt.slotOf(s.me) == pt.leader {
		if v := pt.slots[b[0x24]]; v != nil && v != s.me {
			victim = v
		}
	}
	w.partyMu.Unlock()
	if victim != nil {
		w.partyLeave(victim, true)
	}
	return nil
}

func (s *session) partyInviteSetting(b []byte) error {
	if s.me != nil {
		s.srv.world.partyMu.Lock()
		s.me.invites = b[0x24]
		s.srv.world.partyMu.Unlock()
	}
	return s.c.Send(c101dAck(s.gid, s.lid, b[0x24]))
}

func (s *session) partyLeader(b []byte) error {
	if s.me == nil {
		return nil
	}
	w := s.srv.world
	w.partyMu.Lock()
	pt := s.me.party
	k := int(b[0x24])
	if pt == nil || pt.slotOf(s.me) != pt.leader || k >= 6 || pt.slots[k] == nil {
		w.partyMu.Unlock()
		return nil
	}
	pt.leader = k
	members := pt.members()
	w.partyMu.Unlock()
	for _, m := range members {
		m.conn.Relay(c1006Party(m.gid, m.lid(), 7, uint8(k)))
	}
	return nil
}

var _ = rules.PSU
