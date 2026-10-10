package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
)

// seedCert returns an autocert cache entry (private key, then certificate) for domain, valid for 90 days.
func seedCert(t *testing.T, domain string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "test CA"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

// handshake connects to tc with serverName as SNI ("" sends none) and returns the served leaf's DNS names.
func handshake(t *testing.T, tc *tls.Config, serverName string) ([]string, error) {
	t.Helper()
	c, s := net.Pipe()
	defer c.Close()
	go func() {
		srv := tls.Server(s, tc)
		srv.Handshake()
		srv.Close()
	}()
	cl := tls.Client(c, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	if err := cl.Handshake(); err != nil {
		return nil, err
	}
	return cl.ConnectionState().PeerCertificates[0].DNSNames, nil
}

func TestACME(t *testing.T) {
	const domain = "fuser.example.org"
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	// The directory is unreachable, so the test fails rather than contacting a CA if the seeded certificate isn't used.
	m := newACMEManager(config.ACMEConfig{Domains: []string{domain}, CacheDir: t.TempDir(),
		DirectoryURL: "https://127.0.0.1:1/directory"}, log)
	ctx := context.Background()

	accountKey, _ := pem.Decode(seedCert(t, domain))
	if err := m.Cache.Put(ctx, "acme_account+key", pem.EncodeToMemory(accountKey)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "acme certificate obtained") {
		t.Errorf("the account key was logged as a certificate:\n%s", logs.String())
	}
	if err := m.Cache.Put(ctx, domain+"+token", seedCert(t, domain)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "acme certificate obtained") {
		t.Errorf("a challenge certificate was logged:\n%s", logs.String())
	}
	if err := m.Cache.Put(ctx, domain, seedCert(t, domain)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `acme certificate obtained" key=`+domain) {
		t.Errorf("the stored certificate wasn't logged:\n%s", logs.String())
	}

	tc := acmeTLSConfig(m, domain)
	for _, sni := range []string{domain, ""} {
		names, err := handshake(t, tc, sni)
		if err != nil || len(names) != 1 || names[0] != domain {
			t.Errorf("SNI %q: names %v, err %v", sni, names, err)
		}
	}
	if _, err := handshake(t, tc, "other.example.org"); err == nil {
		t.Error("a host outside acme.domains got a certificate")
	}

	logs.Reset()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	watchACME(cancelled, m, []string{domain}, log)
	if !strings.Contains(logs.String(), `acme certificate ready" domain=`+domain) {
		t.Errorf("startup check didn't report the certificate:\n%s", logs.String())
	}
}
