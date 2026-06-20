package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html web/htmx.min.js web/app.js
var webFiles embed.FS

var appTemplates = template.Must(template.New("app").Funcs(template.FuncMap{
	"humanSize": humanSize,
}).ParseFS(webFiles, "web/index.html"))

type fileApp struct {
	root            string
	maxUploadBytes  int64
	maxStorageBytes int64
	uploadSlots     chan struct{}
	storageMu       sync.Mutex
}

type browserData struct {
	Current     string
	Parent      string
	Breadcrumbs []breadcrumb
	Entries     []fileEntry
	Message     string
	Error       string
	MaxUpload   string
	MaxStorage  string
}

type breadcrumb struct {
	Name string
	Path string
}

type fileEntry struct {
	Name     string
	Path     string
	IsDir    bool
	Size     int64
	Modified time.Time
}

type pendingUpload struct {
	header      *multipart.FileHeader
	destination string
}

func newFileApp(root string, maxUploadBytes int64, maxConcurrentUploads int, maxStorageBytes int64) (http.Handler, error) {
	if maxUploadBytes < 1 || maxConcurrentUploads < 1 || maxStorageBytes < 1 {
		return nil, fmt.Errorf("upload and storage limits must be positive")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute root path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve root symlinks: %w", err)
	}

	app := &fileApp{
		root:            resolvedRoot,
		maxUploadBytes:  maxUploadBytes,
		maxStorageBytes: maxStorageBytes,
		uploadSlots:     make(chan struct{}, maxConcurrentUploads),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/assets/htmx.min.js", app.handleHTMX)
	mux.HandleFunc("/assets/app.js", app.handleAppJS)
	mux.HandleFunc("/api/files", app.handleFiles)
	mux.HandleFunc("/api/upload", app.handleUpload)
	mux.HandleFunc("/download", app.handleDownload)
	return app.securityHeaders(mux), nil
}

func (app *fileApp) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := appTemplates.ExecuteTemplate(w, "index", nil); err != nil {
		logTemplateError(err)
	}
}

func (app *fileApp) handleHTMX(w http.ResponseWriter, r *http.Request) {
	app.serveJavaScript(w, r, "web/htmx.min.js")
}

func (app *fileApp) handleAppJS(w http.ResponseWriter, r *http.Request) {
	app.serveJavaScript(w, r, "web/app.js")
}

func (app *fileApp) serveJavaScript(w http.ResponseWriter, r *http.Request, assetPath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	content, err := webFiles.ReadFile(assetPath)
	if err != nil {
		http.Error(w, "embedded asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(content)
}

func (app *fileApp) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	if err := app.renderBrowser(w, r.URL.Query().Get("path"), "", ""); err != nil {
		app.writePathError(w, err)
	}
}

func (app *fileApp) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin uploads are not allowed", http.StatusForbidden)
		return
	}
	if !app.acquireUploadSlot() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many uploads are currently in progress", http.StatusTooManyRequests)
		return
	}
	defer app.releaseUploadSlot()

	r.Body = http.MaxBytesReader(w, r.Body, app.maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "upload is invalid or exceeds the configured size limit", http.StatusRequestEntityTooLarge)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	current := r.FormValue("path")
	directory, normalized, err := app.resolveExisting(current)
	if err != nil {
		app.writePathError(w, err)
		return
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		http.Error(w, "upload destination is not a directory", http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		app.renderBrowser(w, normalized, "", "Choose at least one file to upload.")
		return
	}

	pending := make([]pendingUpload, 0, len(files))
	destinations := make(map[string]struct{}, len(files))
	for _, header := range files {
		name := safeUploadName(header.Filename)
		if name == "" {
			app.renderBrowser(w, normalized, "", "One of the uploaded files has an invalid name.")
			return
		}
		destination := filepath.Join(directory, name)
		if !pathWithinRoot(app.root, destination) {
			app.renderBrowser(w, normalized, "", "The upload destination is outside the shared folder.")
			return
		}
		if _, duplicate := destinations[destination]; duplicate {
			app.renderBrowser(w, normalized, "", fmt.Sprintf("%s appears more than once in this upload.", name))
			return
		}
		if _, err := os.Lstat(destination); err == nil {
			app.renderBrowser(w, normalized, "", fmt.Sprintf("%s already exists; rename it before uploading", name))
			return
		} else if !os.IsNotExist(err) {
			app.renderBrowser(w, normalized, "", fmt.Sprintf("unable to inspect the destination for %s", name))
			return
		}
		destinations[destination] = struct{}{}
		pending = append(pending, pendingUpload{header: header, destination: destination})
	}

	uploaded, err := app.commitUploads(pending)
	if err != nil {
		app.renderBrowser(w, normalized, "", err.Error())
		return
	}

	message := fmt.Sprintf("Uploaded %d file.", uploaded)
	if uploaded != 1 {
		message = fmt.Sprintf("Uploaded %d files.", uploaded)
	}
	if err := app.renderBrowser(w, normalized, message, ""); err != nil {
		app.writePathError(w, err)
	}
}

func (app *fileApp) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	filePath, _, err := app.resolveExisting(r.URL.Query().Get("path"))
	if err != nil {
		app.writePathError(w, err)
		return
	}
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "unable to open file", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "unable to inspect file", http.StatusInternalServerError)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "directories cannot be downloaded", http.StatusBadRequest)
		return
	}

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()})
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (app *fileApp) renderBrowser(w http.ResponseWriter, requested, message, errorMessage string) error {
	directory, normalized, err := app.resolveExisting(requested)
	if err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}

	directoryEntries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	entries := make([]fileEntry, 0, len(directoryEntries))
	for _, entry := range directoryEntries {
		entryPath := filepath.Join(directory, entry.Name())
		resolvedPath, err := filepath.EvalSymlinks(entryPath)
		if err != nil || !pathWithinRoot(app.root, resolvedPath) {
			continue
		}
		entryInfo, err := os.Stat(resolvedPath)
		if err != nil {
			continue
		}
		entries = append(entries, fileEntry{
			Name:     entry.Name(),
			Path:     joinRelativePath(normalized, entry.Name()),
			IsDir:    entryInfo.IsDir(),
			Size:     entryInfo.Size(),
			Modified: entryInfo.ModTime(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	data := browserData{
		Current:     normalized,
		Parent:      parentPath(normalized),
		Breadcrumbs: makeBreadcrumbs(normalized),
		Entries:     entries,
		Message:     message,
		Error:       errorMessage,
		MaxUpload:   humanSize(app.maxUploadBytes),
		MaxStorage:  humanSize(app.maxStorageBytes),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return appTemplates.ExecuteTemplate(w, "browser", data)
}

func (app *fileApp) acquireUploadSlot() bool {
	select {
	case app.uploadSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (app *fileApp) releaseUploadSlot() {
	<-app.uploadSlots
}

func (app *fileApp) commitUploads(pending []pendingUpload) (int, error) {
	app.storageMu.Lock()
	defer app.storageMu.Unlock()

	currentSize, err := folderSize(app.root)
	if err != nil {
		return 0, fmt.Errorf("unable to calculate shared folder usage")
	}
	uploadSize := int64(0)
	for _, upload := range pending {
		if upload.header.Size < 0 || upload.header.Size > app.maxStorageBytes-uploadSize {
			return 0, fmt.Errorf("the selected files exceed the shared folder storage limit")
		}
		uploadSize += upload.header.Size
	}
	if currentSize > app.maxStorageBytes-uploadSize {
		return 0, fmt.Errorf("upload would exceed the shared folder storage limit of %s", humanSize(app.maxStorageBytes))
	}

	created := make([]string, 0, len(pending))
	for _, upload := range pending {
		if err := saveUpload(upload.header, upload.destination); err != nil {
			for _, createdPath := range created {
				os.Remove(createdPath)
			}
			return 0, err
		}
		created = append(created, upload.destination)
	}
	return len(created), nil
}

func folderSize(root string) (int64, error) {
	total := int64(0)
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > math.MaxInt64-total {
				return fmt.Errorf("shared folder size overflow")
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (app *fileApp) resolveExisting(requested string) (string, string, error) {
	normalized, err := normalizeRelativePath(requested)
	if err != nil {
		return "", "", err
	}
	candidate := filepath.Join(app.root, filepath.FromSlash(normalized))
	resolvedPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", err
	}
	if !pathWithinRoot(app.root, resolvedPath) {
		return "", "", os.ErrPermission
	}
	return resolvedPath, normalized, nil
}

func (app *fileApp) writePathError(w http.ResponseWriter, err error) {
	if os.IsNotExist(err) {
		http.Error(w, "file or directory not found", http.StatusNotFound)
		return
	}
	if os.IsPermission(err) || strings.Contains(err.Error(), "invalid path") {
		http.Error(w, "invalid or forbidden path", http.StatusForbidden)
		return
	}
	http.Error(w, "unable to access the requested path", http.StatusInternalServerError)
}

func (app *fileApp) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func saveUpload(header *multipart.FileHeader, destination string) error {
	source, err := header.Open()
	if err != nil {
		return fmt.Errorf("unable to read an uploaded file")
	}
	defer source.Close()

	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return fmt.Errorf("%s already exists; rename it before uploading", filepath.Base(destination))
	}
	if err != nil {
		return fmt.Errorf("unable to create %s", filepath.Base(destination))
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		os.Remove(destination)
		return fmt.Errorf("unable to save %s", filepath.Base(destination))
	}
	if err := target.Close(); err != nil {
		os.Remove(destination)
		return fmt.Errorf("unable to finish saving %s", filepath.Base(destination))
	}
	return nil
}

func normalizeRelativePath(value string) (string, error) {
	if strings.ContainsRune(value, 0) || filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", fmt.Errorf("invalid path")
	}
	normalized := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if normalized == "." {
		return "", nil
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") || strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("invalid path")
	}
	return normalized, nil
}

func pathWithinRoot(root, target string) bool {
	relativePath, err := filepath.Rel(root, target)
	return err == nil && relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) && !filepath.IsAbs(relativePath)
}

func safeUploadName(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	name := path.Base(value)
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, 0) {
		return ""
	}
	return name
}

func joinRelativePath(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}

func parentPath(value string) string {
	if value == "" {
		return ""
	}
	parent := path.Dir(value)
	if parent == "." {
		return ""
	}
	return parent
}

func makeBreadcrumbs(value string) []breadcrumb {
	crumbs := []breadcrumb{{Name: "Files", Path: ""}}
	if value == "" {
		return crumbs
	}
	current := ""
	for _, part := range strings.Split(value, "/") {
		current = joinRelativePath(current, part)
		crumbs = append(crumbs, breadcrumb{Name: part, Path: current})
	}
	return crumbs
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func humanSize(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor := unit
	exponent := 0
	for quotient := size / unit; quotient >= unit && exponent < 4; quotient /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGTPE"[exponent])
}

func logTemplateError(err error) {
	fmt.Fprintf(os.Stderr, "template rendering error: %v\n", err)
}
