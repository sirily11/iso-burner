package burn

import (
	"context"
	"testing"
)

// TestStreamTypeCompiles runs the Add-Type block of the burn script, which
// Go cannot check, so a C# error fails CI instead of every burn.
func TestStreamTypeCompiles(t *testing.T) {
	script := prologue + `
try {
` + streamType + `
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`
	if err := runLines(powershell(context.Background(), script), func(string) {}); err != nil {
		t.Fatalf("Add-Type failed: %v", err)
	}
}
