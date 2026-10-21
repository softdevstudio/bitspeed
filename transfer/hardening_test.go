package transfer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLargeFileAssemblyAllocation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "large")
	f, e := os.Create(source)
	if e != nil {
		t.Fatal(e)
	}
	const size = int64(64<<20) + 17
	if e = f.Truncate(size); e != nil {
		t.Fatal(e)
	}
	f.Close()
	spool := filepath.Join(dir, "spool")
	m, e := Prepare(context.Background(), source, spool, PrepareOptions{})
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(source)
	r, server := testReceiver(t)
	if _, e = UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s, e := r.finalize(context.Background(), m)
	runtime.ReadMemStats(&after)
	if e != nil || s.State != Completed || s.Receipt == nil || s.Receipt.FileSize != size {
		t.Fatal(s, e)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("64 MiB + 17 byte assembly, full verification and completion allocated %d bytes", allocated)
	if allocated > 16<<20 {
		t.Fatal("assembly allocated with full file size")
	}
}

func TestVerifiedTLSDelivery(t *testing.T) {
	if os.Getenv("BITSPEED_TLS_CHILD") != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestVerifiedTLSDelivery$", "-test.v")
		child.Env = append(os.Environ(), "BITSPEED_TLS_CHILD=1", "GODEBUG=x509usefallbackroots=1")
		if output, e := child.CombinedOutput(); e != nil {
			t.Fatal(e, string(output))
		} else {
			t.Log(strings.TrimSpace(string(output)))
		}
		return
	}
	// Fallback trust is confined to this test process; production clients use
	// platform roots. No insecure verification option is added to the product.
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "BitSpeed local TLS test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	certificate, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	x509.SetFallbackRoots(roots)
	r, plain, spool, m := stagedFile(t, []byte("verified TLS delivery"))
	plain.Close()
	server := httptest.NewUnstartedServer(r)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID)
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	if _, e = QueryStatus(context.Background(), strings.Replace(server.URL, "127.0.0.1", "localhost", 1), testToken, m.TransferID); e == nil {
		t.Fatal("ignored certificate hostname mismatch")
	}
	// A different self-signed issuer must still fail normal certificate checks.
	untrusted := httptest.NewTLSServer(r)
	defer untrusted.Close()
	if _, e = QueryStatus(context.Background(), untrusted.URL, testToken, m.TransferID); e == nil {
		t.Fatal("trusted unrelated certificate")
	}
}
