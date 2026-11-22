package bee

import "testing"

func TestSensitiveEvidenceTokensTable(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		supplied string
		wantErr  bool
	}{
		{name: "uppercase url grounded", value: "endpoint HTTPS://evil.example/api", supplied: "endpoint HTTPS://evil.example/api", wantErr: false},
		{name: "uppercase url ungrounded", value: "endpoint HTTPS://evil.example/api", supplied: "endpoint unavailable", wantErr: true},
		{name: "scheme relative", value: "see //evil.example/path", supplied: "see //evil.example/path", wantErr: false},
		{name: "relative traversal ungrounded", value: "read ../outside", supplied: "read the report", wantErr: true},
		{name: "home path ungrounded", value: "read ~/.ssh/config", supplied: "read config", wantErr: true},
		{name: "unc path ungrounded", value: `read \\server\share\file`, supplied: "read file", wantErr: true},
		{name: "drive path ungrounded", value: `read C:\\secret.txt`, supplied: "read file", wantErr: true},
		{name: "version prose", value: "version 1.2.3 is current", supplied: "", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSensitiveEvidenceTokens(tt.value, tt.supplied, true); (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestMemoryRerankOpaqueIDValidation(t *testing.T) {
	for _, id := range []string{"../../outside", "https://evil.example", "api_key=secret", "../x", "~/secret"} {
		if err := validateOpaqueMemoryID(id, 256); err == nil {
			t.Fatalf("unsafe id %q accepted", id)
		}
	}
	for _, id := range []string{"candidate-a", "candidate_2", "record.3"} {
		if err := validateOpaqueMemoryID(id, 256); err != nil {
			t.Fatalf("safe id %q rejected: %v", id, err)
		}
	}
}
