# Trace storage

`Storage` persists bundles under `{run_id}/{raw|redacted}/{sha256}.jsonl.gz` and exposes Put/Get/Stat/List. HTTP authorization and trace integrity validation remain in the server. Providers do not change object identity or the raw/redacted access-control contract.

- `S3Storage`: AWS SDK, AWS or local RustFS. Existing S3 configuration remains supported for self-hosting/local development.
- `AzureStorage` (#4902): Azure Blob SDK, a private container, bounded 4 MiB upload buffers with concurrency one, streamed downloads, metadata and complete paginated listings sorted by canonical key. Invalid refs fail before network access. BlobNotFound maps to ErrNotFound; missing containers/authorization failures stay operational failures. SDK response bodies, request URLs and connection strings are never included in returned errors because SAS credentials may be embedded in URLs.

Select Azure with `FISHHAWKD_AZURE_BLOB_CONTAINER` (or `--azure-blob-container`) and secret `FISHHAWKD_AZURE_STORAGE_CONNECTION_STRING`. Both are required; setting either with `FISHHAWKD_S3_BUCKET` refuses startup. Azure takes precedence over dev-memory convenience flags. The hosted pilot uses a container-scoped, expiring SAS with read/create/write/list and no delete; provision/rotate it through Key Vault as described in `infra/terraform/README.md`. The adapter also accepts standard Azure connection strings for self-hosted callers; no endpoint rewriting makes Azure emulate S3.

Tests exercise the actual SDK against a local HTTP fixture (block upload, stream, metadata, continuation markers, failures and secret-safe diagnostics); S3 integration tests continue using RustFS. First deployment must additionally round-trip a signed trace through Azure and drill blob version recovery.
