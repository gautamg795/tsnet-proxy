package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestBridgeCopiesBinaryBothDirections(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	input := []byte{0, 1, 2, 255, 0, 9}
	remote := []byte{255, 3, 0, 4}
	go func() {
		got := make([]byte, len(input))
		_, _ = io.ReadFull(server, got)
		if !bytes.Equal(got, input) {
			t.Errorf("remote received %v, want %v", got, input)
		}
		_, _ = server.Write(remote)
		_ = server.Close()
	}()
	var output bytes.Buffer
	if err := Bridge(context.Background(), client, bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), remote) {
		t.Fatalf("stdout = %v, want %v", output.Bytes(), remote)
	}
}

func TestBridgeInputEOFHalfClosesAndDrains(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan []byte, 1)
	go func() {
		conn, _ := listener.Accept()
		defer conn.Close()
		got, _ := io.ReadAll(conn)
		serverDone <- got
		_, _ = conn.Write([]byte("response after EOF"))
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Bridge(context.Background(), client, bytes.NewBufferString("request"), &output); err != nil {
		t.Fatal(err)
	}
	if got := string(<-serverDone); got != "request" {
		t.Fatalf("server got %q", got)
	}
	if got := output.String(); got != "response after EOF" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestBridgeRemoteEOFDoesNotWaitForStdin(t *testing.T) {
	client, server := net.Pipe()
	block := make(chan struct{})
	reader := blockingReader{unblock: block}
	go server.Close()
	done := make(chan error, 1)
	go func() { done <- Bridge(context.Background(), client, reader, io.Discard) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(block)
		t.Fatal("Bridge waited for uncancellable stdin after remote EOF")
	}
	close(block)
}

func TestBridgeCancellationClosesConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	unblock := make(chan struct{})
	started := make(chan struct{}, 1)
	released := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Bridge(ctx, client, blockingReader{unblock: unblock, started: started, released: released}, io.Discard)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Bridge did not begin reading stdin")
	}
	remoteUnblocked := make(chan error, 1)
	go func() {
		_, err := server.Read(make([]byte, 1))
		remoteUnblocked <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Bridge error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Bridge did not promptly return after cancellation")
	}
	select {
	case err := <-remoteUnblocked:
		if err == nil {
			t.Fatal("remote read was not unblocked by connection closure")
		}
	case <-time.After(time.Second):
		t.Fatal("connection closure did not unblock remote I/O")
	}
	close(unblock)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("intentionally uncancellable stdin reader was not released")
	}
}

type blockingReader struct {
	unblock  <-chan struct{}
	started  chan<- struct{}
	released chan<- struct{}
}

func (r blockingReader) Read([]byte) (int, error) {
	if r.started != nil {
		r.started <- struct{}{}
	}
	<-r.unblock
	if r.released != nil {
		close(r.released)
	}
	return 0, io.EOF
}
