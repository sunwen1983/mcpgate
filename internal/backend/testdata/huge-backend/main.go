// Command huge-backend is a test fake for TestStdioBackendDropsChildOnOversizeLine.
// On its first start (no HUGE_MARKER file yet) it answers the first request
// with a single line past the gateway's stdio line cap, then creates the
// marker; after a restart it answers every request with an empty result.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	marker := os.Getenv("HUGE_MARKER")
	sc := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		var req map[string]json.RawMessage
		_ = json.Unmarshal(sc.Bytes(), &req)
		id := json.RawMessage("null")
		if raw, ok := req["id"]; ok {
			id = raw
		}
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			_ = os.WriteFile(marker, []byte("x"), 0644)
			_, _ = w.WriteString(strings.Repeat("x", (10<<20)+1) + "\n")
			_ = w.Flush()
			continue
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", string(id))
		_ = w.Flush()
	}
}
