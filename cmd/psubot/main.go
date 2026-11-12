// SPDX-License-Identifier: GPL-3.0-or-later

// Command psubot plays the PS2 client's side against a server's plain-TCP ports.
//
//	psubot diff -a LOGIN,GAME -b LOGIN,GAME   run one scenario on two servers (the Python
//	                                          reference and psud) and compare every packet
//	psubot load -login ADDR -game ADDR -bots N -secs S [-pid PID] [-ssl]
//	                                          N players moving and chatting in one lobby;
//	                                          with -pid, the server's CPU and memory
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tls "github.com/driftnet-io/insecure-tls"

	"github.com/princebyyourlogic/gurhal/internal/bot"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: psubot diff|load ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "diff":
		err = diff(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "psubot:", err)
		os.Exit(1)
	}
}

// --- diff -------------------------------------------------------------------------------------

type record struct {
	step, who string
	pkts      []wire.Packet
}

func addrs(s string) (string, string, error) {
	login, game, ok := strings.Cut(s, ",")
	if !ok {
		return "", "", fmt.Errorf("%q: want LOGIN,GAME", s)
	}
	return login, game, nil
}

func diff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	a := fs.String("a", "127.0.0.1:32110,127.0.0.1:32061", "reference server login,game")
	b := fs.String("b", "127.0.0.1:22110,127.0.0.1:22061", "server under test login,game")
	tag := fs.String("tag", strconv.FormatInt(time.Now().Unix()%100000, 10), "suffix for the bot names")
	data := fs.String("data", "data", "the game data directory (for the mission scenario)")
	fs.Parse(args)
	ms, err := pickMission(*data)
	if err != nil {
		return err
	}
	var runs [2][]record
	var checks [2][]string
	for i, s := range []string{*a, *b} {
		login, game, err := addrs(s)
		if err != nil {
			return err
		}
		if runs[i], err = scenario(login, game, *tag); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		party, err := partyScenario(login, game, *tag)
		if err != nil {
			return fmt.Errorf("%s party: %w", s, err)
		}
		mrec, problems, err := missionScenario(login, game, *tag, ms)
		if err != nil {
			return fmt.Errorf("%s mission: %w", s, err)
		}
		runs[i] = append(append(runs[i], party...), mrec...)
		checks[i] = problems
	}
	for i, s := range []string{"reference", "under test"} {
		if len(checks[i]) == 0 {
			fmt.Printf("ok   mission %d fight on the %s server: kills, EXP, wave clear, drop pickup, clear, leave\n", ms.qid, s)
		}
		for _, p := range checks[i] {
			fmt.Printf("FAIL mission %d fight on the %s server: %s\n", ms.qid, s, p)
		}
	}
	bad := 0
	for i, ra := range runs[0] {
		rb := runs[1][i]
		if d := compare(ra.pkts, rb.pkts); d != "" {
			bad++
			fmt.Printf("DIFF %-28s %-2s %s\n", ra.step, ra.who, d)
		} else {
			fmt.Printf("ok   %-28s %-2s %d packets: %s\n", ra.step, ra.who, len(ra.pkts), cmds(ra.pkts))
		}
	}
	if len(runs[0]) != len(runs[1]) {
		return fmt.Errorf("the servers ran %d and %d steps", len(runs[0]), len(runs[1]))
	}
	if bad > 0 || len(checks[1]) > 0 {
		return fmt.Errorf("%d of %d steps differ, %d mission checks failed", bad, len(runs[0]), len(checks[1]))
	}
	return nil
}

// scenario: A enters, changes map, zone and quest; B enters and sees A; A chats and
// moves; the social and shop steps; A leaves.
func scenario(login, game, tag string) ([]record, error) {
	var out []record
	pa := &bot.Player{Name: "diffa" + tag, Disc: "psu", LoginAddr: login, GameAddr: game}
	pb := &bot.Player{Name: "diffb" + tag, Disc: "psu", LoginAddr: login, GameAddr: game}
	settle := func() { time.Sleep(400 * time.Millisecond) }
	var ma, mb int
	snap := func(step string, withB bool) {
		settle()
		out = append(out, record{step, "A", pa.Game.Since(ma)})
		ma = pa.Game.Mark()
		if withB {
			out = append(out, record{step, "B", pb.Game.Since(mb)})
			mb = pb.Game.Mark()
		}
	}
	for _, p := range []*bot.Player{pa, pb} {
		if err := p.Login("testplay"); err != nil {
			return nil, err
		}
	}
	if err := pa.Enter(true, 1, 1, 3); err != nil {
		return nil, fmt.Errorf("A enter: %w", err)
	}
	snap("enter", false)
	for _, w := range []struct {
		name    string
		q       uint32
		z, m, e uint16
	}{
		{"map change 0/1", 1100000, 0, 1, 0x14},
		{"zone 3 (Parum)", 1100000, 3, 1, 1},
		{"back to the Colony 2/4", 1101000, 2, 4, 0},
		{"no archives (bounce)", 1150000, 0, 1, 0},
	} {
		if _, err := pa.Warp(w.q, w.z, w.m, w.e); err != nil {
			return nil, fmt.Errorf("A %s: %w", w.name, err)
		}
		snap(w.name, false)
	}
	if err := pb.Enter(true, 2, 0, 7); err != nil {
		return nil, fmt.Errorf("B enter: %w", err)
	}
	mb = 0
	snap("B enters", true)
	pa.Chat("hello from psubot")
	snap("A chats", true)
	pa.Move(1.5, 0, -12.25, 16384)
	snap("A moves", true)
	pb.Game.Send(0x021D, nil)
	snap("B opens the Uni-cube", true)
	for _, line := range []string{"!help", "!who", "!gm", "!gm my mag ran away", "!gm", "!stats"} {
		pb.Chat(line)
		snap("B chats "+line, true)
	}
	if err := social(pa, pb, snap); err != nil {
		return nil, err
	}
	pa.Game.Close()
	settle()
	out = append(out, record{"A leaves", "B", pb.Game.Since(mb)})
	pb.Game.Close()
	return out, nil
}

// normalize blanks what differs between runs by design: the hello's temporary GID, the
// server time and the play time (wall-clock seconds).
func normalize(p wire.Packet) []byte {
	b := append([]byte(nil), p.Body...)
	switch p.Cmd {
	case wire.CmdHello:
		return nil
	case 0x0210:
		if len(b) >= 4 {
			copy(b[len(b)-4:], []byte{0, 0, 0, 0})
		}
	case 0x0115:
		if len(b) >= 0x4C {
			copy(b[0x48:], []byte{0, 0, 0, 0})
		}
	case 0x0201: // a user record's level block: its play time
		if len(b) >= 0x134 {
			copy(b[0x130:], []byte{0, 0, 0, 0})
		}
	case 0x1323: // the room record's visitor log: when each came
		if len(b) >= 0x24+0x5CC {
			stamps(b[0x24+0x5CC:], int(binary.LittleEndian.Uint32(b[0x24+0x5C8:])))
		}
	case 0x1320:
		if len(b) >= 0x2C {
			stamps(b[0x2C:], int(binary.LittleEndian.Uint32(b[0x28:])))
		}
	}
	return b
}

// stamps blanks the dates of n 0x50-byte visitor entries.
func stamps(b []byte, n int) {
	for i := range min(n, 20, len(b)/0x50) {
		clear(b[i*0x50 : i*0x50+8])
	}
}

func compare(a, b []wire.Packet) string {
	if cmds(a) != cmds(b) {
		return fmt.Sprintf("commands\n       ref: %s\n       got: %s", cmds(a), cmds(b))
	}
	var out []string
	for i := range a {
		x, y := normalize(a[i]), normalize(b[i])
		if a[i].Channel != b[i].Channel {
			out = append(out, fmt.Sprintf("#%d %#04x channel %d vs %d", i, a[i].Cmd, a[i].Channel, b[i].Channel))
		}
		if bytes.Equal(x, y) {
			continue
		}
		if len(x) != len(y) {
			out = append(out, fmt.Sprintf("#%d %#04x length %d vs %d", i, a[i].Cmd, len(x), len(y)))
			continue
		}
		for o := range x {
			if x[o] != y[o] {
				end := min(o+16, len(x))
				out = append(out, fmt.Sprintf("#%d %#04x body +%#x: ref % x / got % x", i, a[i].Cmd, o, x[o:end], y[o:end]))
				break
			}
		}
	}
	return strings.Join(out, "\n       ")
}

func cmds(ps []wire.Packet) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = fmt.Sprintf("%04x", p.Cmd)
	}
	return strings.Join(s, " ")
}

// --- load -------------------------------------------------------------------------------------

func load(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	login := fs.String("login", "127.0.0.1:22110", "login server")
	game := fs.String("game", "127.0.0.1:22061", "game server")
	n := fs.Int("bots", 20, "players in the lobby")
	secs := fs.Float64("secs", 30, "how long they play")
	hz := fs.Float64("hz", 4, "moves per second per player")
	pid := fs.Int("pid", 0, "the server's process id, to sample its CPU and memory")
	ssl := fs.Bool("ssl", false, "speak SSLv3 (a server's real login and game ports)")
	missions := fs.Int("missions", 0, "players running a mission over and over")
	data := fs.String("data", "data", "the game data directory (for -missions)")
	fs.Parse(args)
	var ms mission
	if *missions > 0 {
		var err error
		if ms, err = pickMission(*data); err != nil {
			return err
		}
	}
	if *ssl {
		bot.Dialer = func(addr string) (net.Conn, error) {
			return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{
				InsecureSkipVerify: true, MinVersion: tls.VersionSSL30, MaxVersion: tls.VersionSSL30,
				CipherSuites: []uint16{tls.TLS_RSA_WITH_RC4_128_SHA}})
		}
	}

	deadline := time.Now().Add(time.Duration(*secs * float64(time.Second)))
	var entered, failed, dropped, moves, received, clears, missionFails atomic.Int64
	stop := make(chan struct{})
	samples := make(chan sample, 1)
	if *pid > 0 {
		go sampler(*pid, stop, samples)
	}
	tag := strconv.FormatInt(time.Now().Unix()%100000, 10)
	var wg sync.WaitGroup
	for i := range *n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &bot.Player{Name: fmt.Sprintf("load%s%d", tag, i), Disc: "psu", LoginAddr: *login, GameAddr: *game}
			if err := p.Login("testplay"); err != nil {
				failed.Add(1)
				return
			}
			if err := p.Enter(true, byte(i%4), byte(i%2), byte(i%10)); err != nil {
				failed.Add(1)
				return
			}
			entered.Add(1)
			defer p.Game.Close()
			x, z := rand.Float32()*100-50, rand.Float32()*100-50
			tick := time.NewTicker(time.Duration(float64(time.Second) / *hz))
			defer tick.Stop()
			for k := 0; time.Now().Before(deadline); k++ {
				<-tick.C
				if p.Game.Closed() {
					dropped.Add(1)
					return
				}
				x, z = x+rand.Float32()*2-1, z+rand.Float32()*2-1
				p.Move(x, 0, z, uint16(rand.IntN(65536)))
				moves.Add(1)
				if k%int(*hz*10) == 0 {
					p.Chat(fmt.Sprintf("hi from %d", i))
				}
				received.Add(int64(len(p.Game.Since(0))))
				p.Game.Clear()
			}
		}()
		time.Sleep(20 * time.Millisecond)
	}
	for i := range *missions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &bot.Player{Name: fmt.Sprintf("mis%s%d", tag, i), Disc: "psu", LoginAddr: *login, GameAddr: *game}
			if err := p.Login("testplay"); err != nil {
				missionFails.Add(1)
				return
			}
			if err := p.Enter(true, 0, 0, 0); err != nil {
				missionFails.Add(1)
				return
			}
			defer p.Game.Close()
			c, err := missionLoop(p, ms, deadline)
			clears.Add(int64(c))
			if err != nil {
				missionFails.Add(1)
				fmt.Fprintln(os.Stderr, "mission bot:", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	fmt.Printf("bots %d @ %g Hz for %gs: entered %d, failed %d, dropped %d, moves sent %d, packets received %d\n",
		*n, *hz, *secs, entered.Load(), failed.Load(), dropped.Load(), moves.Load(), received.Load())
	if *missions > 0 {
		fmt.Printf("mission bots %d: %d clears of %d, %d bots failed\n", *missions, clears.Load(), ms.qid, missionFails.Load())
	}
	if *pid > 0 {
		s := <-samples
		fmt.Printf("server: RSS start %.0f MB, peak %.0f MB, end %.0f MB; CPU (one core = 100%%) avg %.0f%%, peak %.0f%%\n",
			s.rss0, s.rssPeak, s.rssEnd, s.cpuAvg, s.cpuPeak)
	}
	return nil
}

type sample struct{ rss0, rssPeak, rssEnd, cpuAvg, cpuPeak float64 }

// sampler reads the server's CPU ticks and RSS from /proc once a second.
func sampler(pid int, stop chan struct{}, out chan sample) {
	read := func() (ticks float64, rssMB float64) {
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if i := bytes.LastIndexByte(stat, ')'); i > 0 {
			f := strings.Fields(string(stat[i+1:]))
			if len(f) > 12 {
				u, _ := strconv.ParseFloat(f[11], 64)
				s, _ := strconv.ParseFloat(f[12], 64)
				ticks = u + s
			}
		}
		status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		for _, l := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(l, "VmRSS:") {
				kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
				rssMB = kb / 1024
			}
		}
		return
	}
	const hz = 100 // USER_HZ
	t0, r0 := read()
	s := sample{rss0: r0, rssPeak: r0, rssEnd: r0}
	last, lastT := t0, time.Now()
	var sum float64
	var count int
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t, r := read()
			now := time.Now()
			cpu := (t - last) / hz / now.Sub(lastT).Seconds() * 100
			last, lastT = t, now
			sum += cpu
			count++
			s.cpuPeak = max(s.cpuPeak, cpu)
			s.rssPeak, s.rssEnd = max(s.rssPeak, r), r
		case <-stop:
			if count > 0 {
				s.cpuAvg = sum / float64(count)
			}
			out <- s
			return
		}
	}
}
