// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

func init() {
	handlePublic(0x020D, 0x2C, (*session).keyAuth)
	handlePublic(0x080E, 0x28, (*session).version)
	handle(0x0D06, 0, (*session).characterList)
	handle(0x0D02, rules.RawMin, (*session).createCharacter)
	handle(0x020B, 0x28, (*session).selectCharacter)
	handle(0x0D07, 0x38, (*session).setOptions)
	handle(0x0D04, 0x40, (*session).workWrite)
}

// keyAuth redeems the key login issued: 0x020D +0x24 GID, +0x28 key.
func (s *session) keyAuth(b []byte) error {
	if s.authed {
		return nil
	}
	gid := binary.LittleEndian.Uint32(b[0x24:])
	disc, ok := s.srv.Keys.Take(gid, b[0x28:0x2C])
	if !ok {
		s.c.Log.Info("game: key refused", "gid", fmt.Sprintf("%#08x", gid))
		return errDrop
	}
	if _, banned, err := s.srv.Store.ActiveBan(s.ctx, gid, time.Now()); err != nil || banned {
		s.c.Log.Info("game: refused", "gid", fmt.Sprintf("%#08x", gid), "banned", banned, "err", err)
		return errDrop
	}
	s.gid, s.authed, s.disc, s.r = gid, true, disc, s.srv.Rules.For(disc)
	if acc, err := s.srv.Store.AccountByGID(s.ctx, gid); err == nil {
		s.user = acc.Username
	}
	s.c.Log = s.c.Log.With("gid", fmt.Sprintf("%#08x", gid), "disc", disc)
	s.c.SetIdle(0) // a player may stand still; the keepalive notices a dead peer
	go s.keepalive()
	return s.c.Send(accountFlags(gid))
}

func (s *session) version(b []byte) error {
	s.entrance = b[0x27]
	return nil
}

func (s *session) setOptions(b []byte) error {
	s.options = append([]byte(nil), b[0x24:0x38]...)
	return nil
}

func (s *session) characterList(_ []byte) error {
	s.save() // the list shows the selected character's play time
	chars, err := s.srv.Store.Characters(s.ctx, s.gid)
	if err != nil {
		return err
	}
	var slots [4][]byte
	for _, c := range chars {
		slots[c.Slot] = s.r.SlotRecord(c.Raw, s.stateOf(c))
	}
	return s.c.SendPaced(characterList(s.gid, slots))
}

// createCharacter stores a 0x0D02 body: +0x24 slot, +0x28 UTF-16 name, race, gender,
// type, appearance. No reply.
func (s *session) createCharacter(b []byte) error {
	slot := binary.LittleEndian.Uint32(b[0x24:])
	if slot > 3 {
		return fmt.Errorf("character slot %d", slot)
	}
	name := rules.UTF16Text(b[rules.RawName:rules.RawRace])
	if err := s.srv.Store.CreateCharacter(s.ctx, s.gid, store.Character{Slot: int(slot), Name: name,
		Raw: append([]byte(nil), b...)}); err != nil {
		return err
	}
	s.c.Log.Info("character created", "slot", slot, "name", name, "race", b[rules.RawRace])
	return nil
}

func (s *session) selectCharacter(b []byte) error {
	s.saveNow() // the character played so far
	s.char = nil
	slot := int(binary.LittleEndian.Uint32(b[0x24:]))
	chars, err := s.srv.Store.Characters(s.ctx, s.gid)
	if err != nil {
		return err
	}
	for _, c := range chars {
		if c.Slot == slot {
			s.char = &character{gid: s.gid, slot: slot, raw: c.Raw, st: s.stateOf(c), since: time.Now()}
		}
	}
	if s.char == nil {
		return nil
	}
	s.char.st.Game = s.disc
	s.save()
	if s.me != nil {
		s.me.char.Store(s.char)
	}
	s.c.Log.Info("character selected", "slot", slot, "level", s.char.st.Level, "items", len(s.char.st.Inv))
	s.sendAll(s.loadSequence())
	return nil
}

// stateOf is a stored character's state; a new character gets level 1 and the starter
// pack, saved at once.
func (s *session) stateOf(c store.Character) *rules.State {
	var st rules.State
	if len(c.State) > 0 && json.Unmarshal(c.State, &st) == nil && st.Level > 0 {
		return &st
	}
	fresh := s.r.NewState(c.Raw)
	s.srv.saver.queue(s.gid, c.Slot, fresh)
	return fresh
}

// loadSequence is EGS's character load: character, inventory, party, time, universe,
// cards, blacklist, NPCs, load end.
func (s *session) loadSequence() [][]byte {
	now := time.Now()
	ch, r, gid := s.char, s.r, s.gid
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.bank(now)
	d := r.Derived(ch.raw, ch.st)
	u, _ := s.srv.universe(s.uni)
	players := max(1, s.srv.world.population()[s.uni])
	return s.socialLoad([][]byte{
		c0d01(gid, r.CharBin(ch.raw, ch.st, s.disc), s.options, s.chrWork(nil)),
		wire.Encode(0x0A0A, wire.Body{}.Head(wire.NoLID, gid).Bytes(r.Inventory(ch.st, s.disc))),
		c1006PartyEvent(gid, 5),
		c1005OwnParty(gid, ch.raw, ch.st.Level, min(ch.st.HP, d.MaxHP), d.MaxHP),
		c1006PartyEvent(gid, 12),
		c0210Time(gid, now),
		c0222Universe(gid, u, players),
		c1500OwnCard(gid, ch.raw, ch.slot),
		c1501Cards(gid),
		c1512Blacklist(gid),
		c1602NPCs(gid),
		c021bLoadEnd(gid),
	}, ch.st)
}

// chrWork is the 0x0D01 character-work values: AotI's lobby NPCs read the last offline
// story chapter, and the Spaceport counter reads SAVE_LC00..05.
func (s *session) chrWork(extra map[int]int32) map[int]int32 {
	w := map[int]int32{}
	if s.disc == rules.AotI {
		w[saveQuest] = aotiChapter
	}
	for k, v := range extra {
		w[k] = v
	}
	return w
}

var storyFlag = regexp.MustCompile(`^S(1[3-9]|2[0-9])_FLAG_([0-9]{2}|CLR)$`)

// workWrite answers 0x0D04 (work.chrwork_set / chrflag_set and friends): +0x24 name
// (16 bytes), +0x3C value. Both discs' scripts wait for the ack. The Colony gates write
// SAVE_LC00..05 before the Spaceport warp; S13..S29 story flags are kept per character
// and a request with value 0 reads them back.
func (s *session) workWrite(b []byte) error {
	b = append([]byte(nil), b...)
	value := int32(binary.LittleEndian.Uint32(b[0x3C:]))
	if string(b[0x24:0x2B]) == "SAVE_LC" {
		if i, err := strconv.Atoi(string(b[0x2B:0x2D])); err == nil {
			if s.lcWork == nil {
				s.lcWork = map[int]int32{}
			}
			s.lcWork[i] = value
		}
	}
	if s.char != nil {
		name := wire.Latin1(b[0x24:0x34])
		if storyFlag.MatchString(name) {
			s.char.mu.Lock()
			st := s.char.st
			switch {
			case value != 0 && st.ChrFlags[name] != value:
				if st.ChrFlags == nil {
					st.ChrFlags = map[string]int32{}
				}
				st.ChrFlags[name] = value
				s.saveLocked()
			case value == 0:
				if v, ok := st.ChrFlags[name]; ok {
					binary.LittleEndian.PutUint32(b[0x3C:], uint32(v))
				}
			}
			s.char.mu.Unlock()
		}
	}
	return s.c.Send(c0d04Ack(s.gid, b))
}
