package tests

import (
	"context"
	"testing"

	"github.com/sartoopjj/vpn-over-github/shared"
	"github.com/sartoopjj/vpn-over-github/tests/mocks"
)

// Compile-time assertions: concrete transports must satisfy the interface.
var (
	_ shared.Transport = (*shared.GitSmartHTTPClient)(nil)
	_ shared.Transport = (*shared.GitHubGistClient)(nil)
	_ shared.Transport = (*shared.GitHubContentsClient)(nil)
)

func TestGenerateConnID(t *testing.T) {
	id, err := shared.GenerateConnID()
	if err != nil {
		t.Fatalf("GenerateConnID error: %v", err)
	}
	if id == "" {
		t.Fatal("GenerateConnID returned empty string")
	}
	if len(id) < 16 {
		t.Fatalf("GenerateConnID too short: %q", id)
	}
}

func TestIsChannelEntry(t *testing.T) {
	if !shared.IsChannelEntry(shared.ChannelDescPrefix + "abc") {
		t.Fatal("expected IsChannelEntry to be true for channel description")
	}
	if shared.IsChannelEntry("random description") {
		t.Fatal("expected IsChannelEntry to be false for non-channel description")
	}
}

func TestMockTransportReadWriteRoundtrip(t *testing.T) {
	ctx := context.Background()
	tr := mocks.NewMockTransport()
	chID, err := tr.EnsureChannel(ctx, "")
	if err != nil {
		t.Fatalf("EnsureChannel: %v", err)
	}

	batch := &shared.Batch{Seq: 1, Ts: 123, Frames: []shared.Frame{{ConnID: "c1", Seq: 1, Status: shared.FrameActive}}}
	if err := tr.Write(ctx, chID, shared.ClientBatchFile, batch); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := tr.Read(ctx, chID, shared.ClientBatchFile)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got == nil || got.Seq != 1 || len(got.Frames) != 1 {
		t.Fatalf("unexpected batch read result: %+v", got)
	}
}
