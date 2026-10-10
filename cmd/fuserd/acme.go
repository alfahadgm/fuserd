package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"fuseronline/internal/config"
)

// newACMEManager returns the certificate manager for cfg. Only the tls-alpn-01 challenge is used: autocert adds
// http-01 only once its HTTPHandler is mounted, which fuserd never does.
func newACMEManager(cfg config.ACMEConfig, log *slog.Logger) *autocert.Manager {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      loggingCache{Cache: autocert.DirCache(cfg.CacheDir), log: log},
		HostPolicy: autocert.HostWhitelist(cfg.Domains...),
		Email:      cfg.Email,
	}
	if cfg.DirectoryURL != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.DirectoryURL}
	}
	return m
}

// acmeTLSConfig serves m's certificates and its tls-alpn-01 challenge responses. A handshake without SNI (a client
// connecting by IP address) gets the certificate of fallbackHost instead of failing.
func acmeTLSConfig(m *autocert.Manager, fallbackHost string) *tls.Config {
	tc := m.TLSConfig()
	tc.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" {
			h := *hello
			h.ServerName = fallbackHost
			hello = &h
		}
		return m.GetCertificate(hello)
	}
	return tc
}

// watchACME obtains each domain's certificate at startup, so a failure shows in the log then rather than at a
// player's first connection, and checks once a day that autocert's renewal is keeping up. autocert renews at
// a third of the lifetime remaining (at most 30 days) without logging; a certificate with only a sixth left is
// reported as overdue.
func watchACME(ctx context.Context, m *autocert.Manager, domains []string, log *slog.Logger) {
	check := func(startup bool) {
		for _, d := range domains {
			cert, err := m.GetCertificate(ecdsaHello(d))
			if err != nil {
				log.Error("acme certificate unavailable", "domain", d, "err", err)
				continue
			}
			leaf := cert.Leaf
			if leaf == nil {
				if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
					log.Error("acme certificate unreadable", "domain", d, "err", err)
					continue
				}
			}
			left := time.Until(leaf.NotAfter)
			if left < leaf.NotAfter.Sub(leaf.NotBefore)/6 {
				log.Warn("acme certificate renewal overdue", "domain", d, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
					"days_left", int(left.Hours()/24))
			} else if startup {
				log.Info("acme certificate ready", "domain", d, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
					"issuer", leaf.Issuer.CommonName)
			}
		}
	}
	check(true)
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check(false)
		}
	}
}

// ecdsaHello is the handshake of a client that supports ECDSA, so watchACME asks for the ECDSA certificate real
// clients get rather than autocert's RSA fallback for legacy clients.
func ecdsaHello(domain string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:       domain,
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:  []tls.CurveID{tls.CurveP256},
		CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}
}

// loggingCache logs each certificate autocert stores: the first issue and every renewal.
type loggingCache struct {
	autocert.Cache
	log *slog.Logger
}

func (c loggingCache) Put(ctx context.Context, key string, data []byte) error {
	err := c.Cache.Put(ctx, key, data)
	if strings.HasSuffix(key, "+token") { // a tls-alpn-01 challenge certificate
		return err
	}
	leaf := firstCertificate(data)
	if leaf == nil { // the account key
		return err
	}
	if err != nil {
		c.log.Error("acme certificate not saved; it is reissued after a restart", "key", key, "err", err)
	}
	c.log.Info("acme certificate obtained", "key", key, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
		"issuer", leaf.Issuer.CommonName)
	return err
}

// firstCertificate parses the first CERTIFICATE block of an autocert cache entry (private key, then the chain).
func firstCertificate(data []byte) *x509.Certificate {
	for {
		var b *pem.Block
		if b, data = pem.Decode(data); b == nil {
			return nil
		}
		if b.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil
			}
			return cert
		}
	}
}
