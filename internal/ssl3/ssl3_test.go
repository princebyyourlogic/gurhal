// SPDX-License-Identifier: GPL-3.0-or-later

package ssl3

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	tls "github.com/driftnet-io/insecure-tls"
)

func listen(t *testing.T) string {
	t.Helper()
	cert, err := LoadOrCreate(filepath.Join(t.TempDir(), "ssl3.pem"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := Listen(raw, cert)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return raw.Addr().String()
}

func TestSSLv3RoundTrip(t *testing.T) {
	c, err := tls.Dial("tcp", listen(t), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionSSL30,
		MaxVersion: tls.VersionSSL30, CipherSuites: []uint16{tls.TLS_RSA_WITH_RC4_128_SHA}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st := c.ConnectionState(); st.Version != tls.VersionSSL30 || st.CipherSuite != tls.TLS_RSA_WITH_RC4_128_SHA {
		t.Fatalf("negotiated %#04x/%#04x", st.Version, st.CipherSuite)
	}
	msg := bytes.Repeat([]byte("psu "), 5000) // several records each way
	go c.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo mismatch: %v", err)
	}
}

// The discs' ClientHello: an SSLv3 record, client version 3.0, suite 0x0005 only, null
// compression, no extensions.
func TestBarePS2HelloGetsServerHello(t *testing.T) {
	c, err := net.Dial("tcp", listen(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	body := append([]byte{3, 0}, make([]byte, 32)...)
	rand.Read(body[2:])
	body = append(body, 0, 0, 2, 0x00, 0x05, 1, 0)
	hs := append([]byte{1, 0, byte(len(body) >> 8), byte(len(body))}, body...)
	c.Write(append([]byte{22, 3, 0, byte(len(hs) >> 8), byte(len(hs))}, hs...))

	h := make([]byte, 5)
	if _, err := io.ReadFull(c, h); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, binary.BigEndian.Uint16(h[3:]))
	if _, err := io.ReadFull(c, p); err != nil {
		t.Fatal(err)
	}
	if h[0] != 22 || binary.BigEndian.Uint16(h[1:]) != tls.VersionSSL30 || p[0] != 2 {
		t.Fatalf("not an SSLv3 ServerHello: % x / % x", h, p[:8])
	}
	sid := int(p[38])
	if v, s := binary.BigEndian.Uint16(p[4:]), binary.BigEndian.Uint16(p[39+sid:]); v != tls.VersionSSL30 || s != tls.TLS_RSA_WITH_RC4_128_SHA {
		t.Fatalf("ServerHello version %#04x suite %#04x", v, s)
	}
}

func TestTLS1ClientIsRefused(t *testing.T) {
	_, err := tls.Dial("tcp", listen(t), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10})
	if err == nil {
		t.Fatal("a client without SSLv3 completed a handshake")
	}
}

func TestLoadOrCreateKeepsTheCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ssl3.pem")
	a, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Certificate[0], b.Certificate[0]) {
		t.Fatal("a second load minted a new certificate")
	}
}
