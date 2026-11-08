package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/xfer"
)

// fakeSender answers SendFile the way a background agent does, without a
// network.
type fakeSender struct {
	API
	err error
}

func (f *fakeSender) SendFile(ctx context.Context, to, path string, progress xfer.Progress, status xfer.Status) (*FileResult, error) {
	status("connecting to " + to)
	status("waiting for " + to + " to accept")
	progress(0, 100)
	progress(100, 100)
	if f.err != nil {
		return nil, f.err
	}
	return &FileResult{Name: "x", Bytes: 100, Path: "direct"}, nil
}

func serveFake(t *testing.T, a API) {
	t.Setenv("FERRY_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, a)
	for i := 0; !Running(); i++ {
		if i > 100 {
			t.Fatal("agent did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With `ferry up` running, the command that sends is only a client: the
// agent reads the files, so the client's own process shows no disk reads
// at all. What the agent knows, phase and bytes, must reach the client,
// in order, or a working send looks stuck.
func TestRemoteSendFileReportsStatus(t *testing.T) {
	serveFake(t, &fakeSender{})
	var got []string
	r, err := (&Remote{}).SendFile(context.Background(), "alembic", t.TempDir(),
		func(done, total int64) { got = append(got, fmt.Sprintf("%d/%d", done, total)) },
		func(what string) { got = append(got, what) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"connecting to alembic", "waiting for alembic to accept", "0/100", "100/100"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("client heard %q, want %q", got, want)
	}
	if r.Bytes != 100 {
		t.Fatal(r)
	}
}

// A stall in the agent is still a stall to the client.
func TestRemoteStallKind(t *testing.T) {
	serveFake(t, &fakeSender{err: fmt.Errorf("%w: no data has moved for 1m0s", xfer.ErrStalled)})
	_, err := (&Remote{}).SendFile(context.Background(), "alembic", t.TempDir(), nil, nil)
	if !errors.Is(err, xfer.ErrStalled) {
		t.Fatalf("want ErrStalled through the socket, got %v", err)
	}
}
