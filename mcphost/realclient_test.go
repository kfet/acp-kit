package mcphost

import (
	"encoding/json"
	"testing"
)

func TestInitializeVersionEchoesSupported(t *testing.T) {
	for _, v := range legacyProtocolVersions {
		if got := initializeVersion(json.RawMessage(`{"protocolVersion":"` + v + `"}`)); got != v {
			t.Errorf("%s -> %s", v, got)
		}
	}
	for _, p := range []string{`{"protocolVersion":"1999-01-01"}`, `{}`, `[`, ``} {
		if got := initializeVersion(json.RawMessage(p)); got != mcpProtocolVersion {
			t.Errorf("%q -> %s", p, got)
		}
	}
}

// TestClientDeclaresFilesRealMeta: the _meta a real go-sdk client sends
// carries protocolVersion as a string and clientInfo as an object beside
// clientCapabilities. Decoding _meta as one map of capability objects
// failed on those, so a real client never counted as declaring files.
func TestClientDeclaresFilesRealMeta(t *testing.T) {
	p := json.RawMessage(`{"_meta":{
		"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/files":{"download":{},"upload":{}}}},
		"io.modelcontextprotocol/clientInfo":{"name":"fir","version":"dev"},
		"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`)
	if d, dl := clientDeclaresFiles(p); !d || !dl {
		t.Fatalf("declared=%v download=%v", d, dl)
	}
	if d, _ := clientDeclaresFiles(json.RawMessage(`{"_meta":{"io.modelcontextprotocol/clientCapabilities":"x"}}`)); d {
		t.Fatal("malformed clientCapabilities counted as a declaration")
	}
}
