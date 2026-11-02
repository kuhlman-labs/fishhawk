package main

import (
	"strings"
	"testing"
)

func TestServeAzureStorageSelection(t *testing.T) {
	connection := "BlobEndpoint=https://example.blob.core.windows.net/;SharedAccessSignature=sv=2023-11-03&sig=do-not-log-this"
	for _, tt := range []struct{ name, container, connection, s3, want string }{
		{"azure", "traces", connection, "", "Azure trace store configured"},
		{"missing connection", "traces", "", "", "Azure trace store configuration failed"},
		{"missing container", "", connection, "", "Azure trace store configuration failed"},
		{"invalid connection", "traces", "do-not-log-this", "", "Azure trace store configuration failed"},
		{"conflicting providers", "traces", connection, "bucket", "Azure Blob and S3 trace storage cannot both be configured"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FISHHAWKD_AZURE_BLOB_CONTAINER", tt.container)
			t.Setenv("FISHHAWKD_AZURE_STORAGE_CONNECTION_STRING", tt.connection)
			t.Setenv("FISHHAWKD_S3_BUCKET", tt.s3)
			code, log := serveWithProfile(t, "-db", "", bootstrapAbortFlag)
			if code != exitFailure || !strings.Contains(log, tt.want) {
				t.Fatalf("code=%d want log %q; got %s", code, tt.want, log)
			}
			if strings.Contains(log, "do-not-log-this") {
				t.Fatal("credential leaked into startup log")
			}
		})
	}
}
