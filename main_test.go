package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedSPAAndHTMXAsset(t *testing.T) {
	app := testFileApp(t, t.TempDir(), 1<<20)

	page := httptest.NewRecorder()
	app.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("expected SPA response, got %d", page.Code)
	}
	if body := page.Body.String(); !strings.Contains(body, "File Shuttle") || !strings.Contains(body, `hx-get="api/files"`) {
		t.Fatalf("expected embedded HTMX application shell, got %q", body)
	}

	for _, assetPath := range []string{"/assets/htmx.min.js", "/assets/app.js"} {
		asset := httptest.NewRecorder()
		app.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, assetPath, nil))
		if asset.Code != http.StatusOK || asset.Body.Len() < 500 {
			t.Fatalf("expected embedded asset %s, got status %d and %d bytes", assetPath, asset.Code, asset.Body.Len())
		}
	}
}

func TestBrowserListsFoldersAndFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report #1.pdf"), []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := testFileApp(t, root, 1<<20)

	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "documents") || !strings.Contains(body, "report #1.pdf") {
		t.Fatalf("expected folder and file listing, got status %d and body %q", response.Code, body)
	}
	if !strings.Contains(body, "download?path=report&#43;%231.pdf") {
		t.Fatalf("expected safely encoded download URL, got %q", body)
	}
}

func TestAppWorksBehindURLPrefix(t *testing.T) {
	app := testFileApp(t, t.TempDir(), 1<<20)
	mux := http.NewServeMux()
	mux.Handle("/shared/", http.StripPrefix("/shared", app))

	for _, target := range []string{"/shared/", "/shared/api/files", "/shared/assets/htmx.min.js"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("expected prefixed route %s to work, got %d", target, response.Code)
		}
	}
}

func TestUploadAndDownload(t *testing.T) {
	root := t.TempDir()
	app := testFileApp(t, root, 1<<20)
	upload := multipartRequest(t, "/api/upload", "", "notes.txt", []byte("hello from upload"))
	upload.Host = "files.test"
	upload.Header.Set("Origin", "http://files.test")

	uploadResponse := httptest.NewRecorder()
	app.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusOK || !strings.Contains(uploadResponse.Body.String(), "Uploaded 1 file") {
		t.Fatalf("expected successful upload, got status %d and body %q", uploadResponse.Code, uploadResponse.Body.String())
	}
	stored, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "hello from upload" {
		t.Fatalf("unexpected uploaded content %q", stored)
	}

	downloadResponse := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/download?path="+url.QueryEscape("notes.txt"), nil)
	app.ServeHTTP(downloadResponse, request)
	if downloadResponse.Code != http.StatusOK || downloadResponse.Body.String() != "hello from upload" {
		t.Fatalf("expected uploaded file download, got status %d and body %q", downloadResponse.Code, downloadResponse.Body.String())
	}
	if disposition := downloadResponse.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "notes.txt") {
		t.Fatalf("expected attachment content disposition, got %q", disposition)
	}
}

func TestUploadDoesNotOverwriteExistingFile(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(filePath, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := testFileApp(t, root, 1<<20)

	response := httptest.NewRecorder()
	app.ServeHTTP(response, multipartRequest(t, "/api/upload", "", "notes.txt", []byte("replacement")))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "already exists") {
		t.Fatalf("expected duplicate warning, got status %d and body %q", response.Code, response.Body.String())
	}
	stored, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "original" {
		t.Fatalf("existing file was overwritten with %q", stored)
	}
}

func TestMultiFileUploadIsPreflightedAsBatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := testFileApp(t, root, 1<<20)
	request := multipartFilesRequest(t, "/api/upload", "", []uploadFixture{
		{name: "new.txt", content: []byte("new")},
		{name: "existing.txt", content: []byte("replacement")},
	})

	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "already exists") {
		t.Fatalf("expected batch rejection, got status %d and body %q", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file should not be created when batch validation fails")
	}
}

func TestPathsCannotEscapeSharedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, filepath.Join(root, "secret.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	app := testFileApp(t, root, 1<<20)

	for _, target := range []string{
		"/download?path=" + url.QueryEscape("../secret.txt"),
		"/download?path=" + url.QueryEscape("secret.txt"),
	} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusForbidden {
			t.Fatalf("expected forbidden response for %s, got %d", target, response.Code)
		}
	}
}

func TestUploadLimitAndOriginChecks(t *testing.T) {
	app := testFileApp(t, t.TempDir(), 128)

	oversized := multipartRequest(t, "/api/upload", "", "large.txt", bytes.Repeat([]byte("x"), 512))
	response := httptest.NewRecorder()
	app.ServeHTTP(response, oversized)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected upload limit response, got %d", response.Code)
	}

	crossOrigin := multipartRequest(t, "/api/upload", "", "small.txt", []byte("small"))
	crossOrigin.Host = "files.test"
	crossOrigin.Header.Set("Origin", "https://attacker.test")
	response = httptest.NewRecorder()
	app.ServeHTTP(response, crossOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected cross-origin upload rejection, got %d", response.Code)
	}
}

func TestConcurrentUploadSlots(t *testing.T) {
	app := &fileApp{uploadSlots: make(chan struct{}, 2)}
	if !app.acquireUploadSlot() || !app.acquireUploadSlot() {
		t.Fatal("expected configured upload slots to be available")
	}
	if app.acquireUploadSlot() {
		t.Fatal("expected upload above concurrency limit to be rejected")
	}
	response := httptest.NewRecorder()
	app.handleUpload(response, httptest.NewRequest(http.MethodPost, "/api/upload", nil))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("expected HTTP 429 with retry guidance, got %d", response.Code)
	}
	app.releaseUploadSlot()
	if !app.acquireUploadSlot() {
		t.Fatal("expected released upload slot to become available")
	}
}

func TestStorageLimitRejectsUpload(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("123456"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := newFileApp(root, 1<<20, 2, 10)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	app.ServeHTTP(response, multipartRequest(t, "/api/upload", "", "new.txt", []byte("12345")))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "storage limit") {
		t.Fatalf("expected storage quota warning, got status %d and body %q", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("file should not be created when storage quota is exceeded")
	}
}

func TestFolderSizeCountsNestedFilesAndSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "two.txt"), []byte("4567"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "one.txt"), filepath.Join(root, "duplicate.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	size, err := folderSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if size != 7 {
		t.Fatalf("expected 7 bytes of stored files, got %d", size)
	}
}

func TestGenerateSelfSignedCertSecuresKey(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "cert", "server.pem")
	keyPath := filepath.Join(root, "private", "server.key")

	if err := generateSelfSignedCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := keyInfo.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("expected private key permissions 0600, got %04o", permissions)
	}
}

func TestEnsureTLSCertificateGeneratesMissingPair(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "certs", "server.pem")
	keyPath := filepath.Join(root, "certs", "server.key")

	generated, err := ensureTLSCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("expected a missing certificate pair to be generated")
	}
	for _, outputPath := range []string{certPath, keyPath} {
		if _, err := os.Stat(outputPath); err != nil {
			t.Fatalf("expected generated TLS file %s: %v", outputPath, err)
		}
	}

	generated, err = ensureTLSCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Fatal("expected an existing certificate pair to be reused")
	}
}

func TestEnsureTLSCertificateRejectsPartialPair(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "server.pem")
	if err := os.WriteFile(certPath, []byte("certificate"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureTLSCertificate(certPath, filepath.Join(root, "server.key")); err == nil {
		t.Fatal("expected a partial certificate pair to be rejected")
	}
}

func TestEnsureTLSCertificateRejectsInvalidPair(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "server.pem")
	keyPath := filepath.Join(root, "server.key")
	if err := os.WriteFile(certPath, []byte("invalid certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("invalid key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureTLSCertificate(certPath, keyPath); err == nil {
		t.Fatal("expected an invalid certificate pair to be rejected")
	}
}

func testFileApp(t *testing.T, root string, maxUploadBytes int64) http.Handler {
	t.Helper()
	handler, err := newFileApp(root, maxUploadBytes, 4, 10<<30)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func multipartRequest(t *testing.T, target, directory, name string, content []byte) *http.Request {
	t.Helper()
	return multipartFilesRequest(t, target, directory, []uploadFixture{{name: name, content: content}})
}

type uploadFixture struct {
	name    string
	content []byte
}

func multipartFilesRequest(t *testing.T, target, directory string, files []uploadFixture) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("path", directory); err != nil {
		t.Fatal(err)
	}
	for _, upload := range files {
		file, err := writer.CreateFormFile("files", upload.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(file, bytes.NewReader(upload.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}
