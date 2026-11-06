package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestHealthCLI(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"health", "--timeout", "0s"}, 2},
		{[]string{"health", "--timeout", "bad"}, 2},
		{[]string{"health", "extra"}, 2},
		{[]string{"health"}, 1},
	} {
		var out, stderr bytes.Buffer
		dir := t.TempDir()
		code := run(context.Background(), tc.args, bytes.NewReader(nil), &out, &stderr, func(k string) (string, bool) { return dir, k == "WHATSAPP_MCP_DATA_DIR" })
		if code != tc.code {
			t.Fatalf("%v: code=%d stderr=%s", tc.args, code, &stderr)
		}
		if code == 1 {
			var v map[string]any
			if json.Unmarshal(out.Bytes(), &v) != nil || v["ok"] != false {
				t.Fatalf("output=%s", &out)
			}
		}
	}
}
