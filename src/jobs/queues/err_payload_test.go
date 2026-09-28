package queues

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestErrPayloadIsValidJSONWithQuotes(t *testing.T) {
	err := fmt.Errorf("zernio: unexpected status %q", `bad "value"`)
	var got map[string]string
	if uerr := json.Unmarshal([]byte(errPayload(err)), &got); uerr != nil {
		t.Fatalf("errPayload produced invalid JSON: %v", uerr)
	}
	if got["error"] != err.Error() {
		t.Fatalf("error = %q, want %q", got["error"], err.Error())
	}
}
