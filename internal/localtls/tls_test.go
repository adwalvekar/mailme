package localtls

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalTLSAndBindRestrictions(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if err = cert.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"0.0.0.0:1993", ":1993", "[::]:1993", "192.168.0.1:1993", "localhost:1993"} {
		if ln, err := Listen(address, cfg); err == nil {
			ln.Close()
			t.Fatalf("accepted %s", address)
		}
	}
	info, _ := os.Stat(filepath.Join(dir, "localhost.key"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("private key permissions")
	}
	again, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Certificates[0].Certificate[0]) != string(cfg.Certificates[0].Certificate[0]) {
		t.Fatal("certificate changed")
	}
}
