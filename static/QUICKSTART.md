# File Shuttle Quick Start

File Shuttle is a self-contained HTTPS file server with a browser interface for uploading, browsing, and downloading files.

## Run

```bash
make run
```

Open `https://127.0.0.1:8080`. A browser warning is expected because the server automatically creates a self-signed certificate for local use.

Files are shared from `./static` by default. Share another folder with:

```bash
./bin/serve -dir=/path/to/documents
```

To use unencrypted HTTP explicitly:

```bash
./bin/serve -insecure
```

Use the web interface to select files, upload them, navigate folders, or download listed documents. Existing files are not overwritten.
