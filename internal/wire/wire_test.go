// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestEncodePadsAndRoundTrips(t *testing.T) {
	pkt := Encode(0x0202, []byte{1, 2, 3, 4, 5})
	want := []byte{16, 0, 0, 0, 0x02, 0x02, 3, 0, 1, 2, 3, 4, 5, 0, 0, 0}
	if !bytes.Equal(pkt, want) {
		t.Fatalf("Encode = % x, want % x", pkt, want)
	}
	p, err := Read(bytes.NewReader(pkt), 64)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cmd != 0x0202 || p.Channel != ChanServer || !bytes.Equal(p.Body, want[8:]) {
		t.Fatalf("Read = %+v", p)
	}
}

func TestReadRejectsBadSizes(t *testing.T) {
	for _, n := range []uint32{0, 7, 65} {
		var b [HeaderSize]byte
		binary.LittleEndian.PutUint32(b[:], n)
		if _, err := Read(bytes.NewReader(b[:]), 64); !errors.Is(err, ErrSize) {
			t.Errorf("size %d: err = %v, want ErrSize", n, err)
		}
	}
}

func TestKeepaliveIsAnEmptyPacket(t *testing.T) {
	p, err := Read(bytes.NewReader([]byte{8, 0, 0, 0, 0, 0, 0, 0}), 64)
	if err != nil || p.Cmd != 0 || len(p.Body) != 0 {
		t.Fatalf("keepalive: %+v, %v", p, err)
	}
}

func TestFragmentAck(t *testing.T) {
	p, err := Read(bytes.NewReader(EncodeOn(ChanClient, CmdFragmentAck, nil)), 64)
	if err != nil || !p.IsFragmentAck() {
		t.Fatalf("ack: %+v, %v", p, err)
	}
}

func TestFragments(t *testing.T) {
	small := Encode(0x0D03, make([]byte, 100))
	if f := Fragments(small); len(f) != 1 || &f[0][0] != &small[0] {
		t.Fatal("a packet within the limit must go out as is")
	}

	pkt := Encode(0x0D03, bytes.Repeat([]byte{0xAB}, 2*FragmentLimit+92))
	frags := Fragments(pkt)
	if len(frags) != 3 {
		t.Fatalf("got %d fragments, want 3", len(frags))
	}
	var joined []byte
	for i, f := range frags {
		if int(binary.LittleEndian.Uint32(f)) != len(f) || binary.BigEndian.Uint16(f[4:]) != CmdFragment {
			t.Fatalf("fragment %d header % x", i, f[:8])
		}
		if int(binary.LittleEndian.Uint32(f[8:])) != len(pkt) || int(binary.LittleEndian.Uint32(f[12:])) != i*FragmentLimit {
			t.Fatalf("fragment %d total/offset % x", i, f[8:16])
		}
		joined = append(joined, f[16:]...)
	}
	if !bytes.Equal(joined, pkt) {
		t.Fatal("fragments do not reassemble to the packet")
	}
}
