# File Shuttle

A self-contained HTTP/HTTPS file sharing server written in Go. Its single-page web interface lets users browse the nominated folder, navigate subfolders, upload documents, and download files.

The UI and HTMX runtime are embedded in the compiled binary, so no separate frontend installation or internet connection is required. By default, the app shares `./static` at `https://127.0.0.1:8080` and generates a self-signed certificate when no certificate files exist.

## Features
- Responsive single-page file browser
- Multi-file uploads into the current folder
- Explicit download links with attachment headers
- Subfolder navigation without full-page reloads
- Configurable request, concurrency, and shared-folder storage limits
- Embedded HTMX 2.0.10 and HTML/CSS assets
- HTTPS by default with automatically generated or supplied certificates

## Requirements
- Go 1.20 (as declared in `go.mod`)

## CLI Flags
- `-p` : port to bind to (default `8080`)
- `-dir` : directory to serve (default `static`)
- `-url` : optional URL token to obfuscate the root resource; supply an alpha-numeric token (e.g. `-url=files` will serve under `/files/`).
- `-insecure` : explicitly disable TLS and serve unencrypted HTTP
- `-cert` : path to TLS certificate PEM file (default `certs/server.pem`)
- `-key` : path to TLS private key PEM file (default `certs/server.key`)
- `-gencert` : generate a self-signed certificate and key at the `-cert`/`-key` paths, then exit
- `-host` : host/interface to bind to (default `127.0.0.1`); use `0.0.0.0` to listen on all interfaces
- `-max-upload` : maximum total size of one upload request in MB (default `100`)
- `-max-concurrent-uploads` : maximum uploads processed at once (default `4`)
- `-max-storage` : maximum total size of regular files in the shared folder, including subfolders, in MB (default `10240`, or 10 GiB)

Notes:
- The `-url` token must be a non-empty alphanumeric string. The server will normalize it to a path that starts and ends with `/` (e.g. `/token/`).

## Makefile targets
The repository includes a `Makefile` with useful shortcuts:

- `make build` — build the binary into `bin/serve`
- `make run` — build and run the HTTPS server
- `make run-secure` — alias for building and running the default HTTPS server
- `make run-insecure` — build and run with unencrypted HTTP
- `make gen-cert` — generate self-signed cert/key into `certs/server.pem` and `certs/server.key`
- `make fmt` — run `gofmt -w .`
- `make test` — run the test suite
- `make vet` — run `go vet ./...`
- `make clean-binaries` — remove compiled binaries from the project root and `bin/`
- `make clean` — remove compiled binaries and generated certificates

## Examples

Start the web UI and share `./static` on port 8080:

```bash
make run
```

Then open `https://127.0.0.1:8080`. Your browser will warn about the automatically generated self-signed certificate; this is expected for local use.

Share another folder, allow access from the local network, limit each upload request to 250 MB, and cap storage at 20 GiB:

```bash
./bin/serve -dir=/path/to/documents -host=0.0.0.0 -max-upload=250 -max-storage=20480
```

Mount the application under a private-looking URL path:

```bash
./bin/serve -dir=/path/to/documents -url=teamfiles
```

Then open `https://127.0.0.1:8080/teamfiles/`. The URL token is convenience, not authentication.

Start HTTPS on port 443, serving `/tmp` under `/files/` with an existing certificate pair:

```bash
./bin/serve -p=443 -dir=/tmp -url=files -cert=certs/server.pem -key=certs/server.key
```

If both certificate files are absent, the server generates them automatically. To run without TLS explicitly:

```bash
./bin/serve -dir=/tmp -insecure
```

## Generating a certificate manually (optional)
You can also create a self-signed certificate with OpenSSL if you prefer:

```bash
mkdir -p certs
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
	-keyout certs/server.key -out certs/server.pem \
	-subj "/CN=localhost"
```

## Security notes
- HTTPS is enabled by default. When both configured certificate files are absent, the server generates a self-signed pair intended for local development and testing only.
- Use `-insecure` only on a trusted system or network where unencrypted file transfers are acceptable.
- For production use, provide certificates from a trusted CA with `-cert` and `-key`.
- The server listens only on localhost by default. Binding to `0.0.0.0` makes the served files available to other devices allowed by your firewall and network configuration.
- The `-url` option changes the path but is not authentication. Do not rely on it to protect sensitive files.
- Uploads are bounded by `-max-upload` and `-max-concurrent-uploads`, constrained to the shared folder, and rejected when a destination file already exists.
- Before committing an upload, the server measures regular files under the shared folder and rejects the batch if it would exceed `-max-storage`. Symlinks are excluded from this calculation.
- Symlinks that resolve outside the shared folder are not listed or downloadable.
- Avoid running the binary as root to bind privileged ports; instead use a reverse proxy or appropriate capability tools.

## Frontend dependency
HTMX 2.0.10 is vendored in `web/htmx.min.js` and embedded at build time. Its upstream license is included in `web/HTMX-LICENSE`.

## License
See the `LICENSE` file for licensing information.
