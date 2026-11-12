// SPDX-License-Identifier: GPL-3.0-or-later

// Package ssl3 terminates the clients' SSL 3.0 on the login and game ports. Both PS2
// discs link RSA's SSL-C and offer exactly one suite, 0x0005 (RSA key exchange, RC4-128,
// SHA-1 MAC), and accept any RSA certificate. This is compatibility with a 2006 client,
// not security. Go's crypto/tls dropped SSLv3 in 1.14; insecure-tls is a minimal-diff
// fork of it that keeps the SSLv3 server.
package ssl3

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	tls "github.com/driftnet-io/insecure-tls"
)

// Listen wraps ln so that every accepted connection speaks SSLv3 with cert. The
// handshake runs on first use, or explicitly via the connection's HandshakeContext.
func Listen(ln net.Listener, cert tls.Certificate) net.Listener {
	return tls.NewListener(ln, Config(cert))
}

// Config is the server configuration the discs need: SSLv3 only, RC4-SHA only.
func Config(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{cert},
		MinVersion:             tls.VersionSSL30,
		MaxVersion:             tls.VersionSSL30,
		CipherSuites:           []uint16{tls.TLS_RSA_WITH_RC4_128_SHA},
		SessionTicketsDisabled: true,
	}
}

// LoadOrCreate reads the PEM certificate and key at path, first minting a self-signed
// RSA-1024 pair there if the file does not exist.
func LoadOrCreate(path string) (tls.Certificate, error) {
	pemData, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if pemData, err = mint(); err == nil {
			err = writeAtomic(path, pemData)
		}
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("ssl3: certificate %s: %w", path, err)
	}
	cert, err := tls.X509KeyPair(pemData, pemData)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("ssl3: certificate %s: %w", path, err)
	}
	return cert, nil
}

func mint() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gurhal"},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})...), nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
