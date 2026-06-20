package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	// Set CLI variables to hold config data
	port := flag.String("p", "8080", "port to serve files on")
	directory := flag.String("dir", "static", "the directory of static files to host")
	urlToken := flag.String("url", "/", "optional alphanumeric URL path prefix")

	// HTTPS is enabled by default; insecure HTTP must be explicitly requested.
	insecure := flag.Bool("insecure", false, "serve over HTTP without TLS")
	cert := flag.String("cert", "certs/server.pem", "location of SSL public cert")
	key := flag.String("key", "certs/server.key", "location of the SSL private key")
	genCert := flag.Bool("gencert", false, "generate a self-signed cert/key and exit")
	host := flag.String("host", "127.0.0.1", "host/interface to bind to")
	maxUploadMB := flag.Int64("max-upload", 100, "maximum upload request size in MB")
	maxConcurrentUploads := flag.Int("max-concurrent-uploads", 4, "maximum number of uploads processed concurrently")
	maxStorageMB := flag.Int64("max-storage", 10*1024, "maximum shared folder size in MB")

	flag.Usage = func() {
		fmt.Println("Serve is an HTTPS file server used to share files from a local folder ('static' by default).")
		fmt.Println("TLS is enabled by default. Use -insecure only when unencrypted HTTP is explicitly required.")
		fmt.Println("To obfuscate the root url, specific a string to prevent accidental downloading by casual users.")
		fmt.Println()
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])

		flag.PrintDefaults()
	}

	flag.Parse()

	// Validate that CLI flags make sense
	// Port numbers have to be positive integers between 0-65535
	myPort, err := strconv.Atoi(*port)
	if err != nil {
		log.Fatalf("invalid port value %q: must be an integer", *port)
	}
	if myPort < 1 || myPort > 65535 {
		log.Fatalf("port has to be a positive integer between 1 and 65535 (got %d)", myPort)
	}
	if *maxUploadMB < 1 {
		log.Fatal("max-upload has to be at least 1 MB")
	}
	if *maxUploadMB > math.MaxInt64>>20 {
		log.Fatal("max-upload is too large")
	}
	if *maxConcurrentUploads < 1 {
		log.Fatal("max-concurrent-uploads has to be at least 1")
	}
	if *maxStorageMB < 1 {
		log.Fatal("max-storage has to be at least 1 MB")
	}
	if *maxStorageMB > math.MaxInt64>>20 {
		log.Fatal("max-storage is too large")
	}

	// Directory has to exist and be accessible
	cleanedDir := filepath.Clean(*directory)
	dirInfo, err := os.Stat(cleanedDir)
	if err != nil {
		log.Fatalf("cannot access folder %q: %v", cleanedDir, err)
	}
	if !dirInfo.IsDir() {
		log.Fatalf("path %q is not a folder", cleanedDir)
	}

	// URI location has to start and end with '/' to be correctly parsed.
	if *urlToken != "/" {
		token := strings.Trim(*urlToken, "/")
		if token == "" {
			log.Fatal("url token cannot be empty")
		}
		isAlphaNumeric := regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(token)
		if !isAlphaNumeric {
			log.Fatal("url is limited to alpha-numeric characters only")
		}
		*urlToken = fmt.Sprintf("/%s/", token)
	}

	handler, err := newFileApp(cleanedDir, *maxUploadMB<<20, *maxConcurrentUploads, *maxStorageMB<<20)
	if err != nil {
		log.Fatalf("failed to prepare folder %q: %v", cleanedDir, err)
	}
	mux := http.NewServeMux()
	if *urlToken == "/" {
		mux.Handle("/", handler)
	} else {
		mux.Handle(*urlToken, http.StripPrefix(strings.TrimSuffix(*urlToken, "/"), handler))
	}

	// If requested, generate a self-signed certificate at the specified paths.
	if *genCert {
		if err := generateSelfSignedCert(*cert, *key); err != nil {
			log.Fatalf("failed to generate certificate: %v", err)
		}
		log.Printf("generated self-signed cert=%s key=%s", *cert, *key)
		return
	}
	if !*insecure {
		generated, err := ensureTLSCertificate(*cert, *key)
		if err != nil {
			log.Fatalf("failed to prepare TLS certificate: %v", err)
		}
		if generated {
			log.Printf("generated self-signed TLS certificate cert=%s key=%s", *cert, *key)
		}
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(myPort))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Minute,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	// Setup graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if *insecure {
			log.Printf("Starting HTTP server for folder '%s' on %s", cleanedDir, addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server error: %v", err)
			}
		} else {
			log.Printf("Starting HTTPS server for folder '%s' on %s", cleanedDir, addr)
			if err := srv.ListenAndServeTLS(*cert, *key); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTPS server error: %v", err)
			}
		}
	}()

	// Wait for interrupt
	<-stop
	log.Printf("shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server shutdown failed: %v", err)
	}
	log.Printf("server gracefully stopped")
}

func ensureTLSCertificate(certPath, keyPath string) (bool, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return false, fmt.Errorf("invalid TLS certificate pair: %w", err)
		}
		return false, nil
	}
	if os.IsNotExist(certErr) && os.IsNotExist(keyErr) {
		if err := generateSelfSignedCert(certPath, keyPath); err != nil {
			return false, err
		}
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return false, fmt.Errorf("generated an invalid TLS certificate pair: %w", err)
		}
		return true, nil
	}
	if certErr != nil && !os.IsNotExist(certErr) {
		return false, fmt.Errorf("cannot access certificate %s: %w", certPath, certErr)
	}
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return false, fmt.Errorf("cannot access private key %s: %w", keyPath, keyErr)
	}
	return false, fmt.Errorf("certificate and private key must either both exist or both be absent")
}

// generateSelfSignedCert creates a self-signed certificate and writes PEM files.
func generateSelfSignedCert(certPath, keyPath string) error {
	for _, outputPath := range []string{certPath, keyPath} {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", outputPath, err)
		}
	}

	// Generate RSA key
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Local Development"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing: %w", certPath, err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		certOut.Close()
		return fmt.Errorf("failed to write certificate: %w", err)
	}
	if err := certOut.Close(); err != nil {
		return fmt.Errorf("failed to close certificate: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing: %w", keyPath, err)
	}
	if err := keyOut.Chmod(0o600); err != nil {
		keyOut.Close()
		return fmt.Errorf("failed to secure private key permissions: %w", err)
	}
	privBytes := x509.MarshalPKCS1PrivateKey(priv)
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes}); err != nil {
		keyOut.Close()
		return fmt.Errorf("failed to write private key: %w", err)
	}
	if err := keyOut.Close(); err != nil {
		return fmt.Errorf("failed to close private key: %w", err)
	}

	return nil
}
