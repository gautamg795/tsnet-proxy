// Package proxy bridges an OpenSSH ProxyCommand byte stream to a net.Conn.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// Bridge copies stdin to conn and conn to stdout concurrently. It never closes
// in or out. EOF on input half-closes TCP when the connection supports it, then
// continues draining the remote side.
func Bridge(ctx context.Context, conn net.Conn, in io.Reader, out io.Writer) error {
	inDone := make(chan error, 1)
	remoteDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, in)
		if err == nil {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				err = cw.CloseWrite()
			}
		}
		inDone <- err
	}()
	go func() {
		_, err := io.Copy(out, conn)
		remoteDone <- err
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case err := <-remoteDone:
		// Do not wait for stdin: an arbitrary process reader cannot be
		// cancelled. Closing the network side bounds resources and lets the
		// command return as soon as stdout has drained.
		_ = conn.Close()
		if isNormal(err) {
			return nil
		}
		return fmt.Errorf("copy remote to stdout: %w", err)
	case err := <-inDone:
		if !isNormal(err) {
			_ = conn.Close()
			return fmt.Errorf("copy stdin to remote: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return ctx.Err()
		case remoteErr := <-remoteDone:
			_ = conn.Close()
			if isNormal(remoteErr) {
				return nil
			}
			return fmt.Errorf("copy remote to stdout: %w", remoteErr)
		}
	}
}

func isNormal(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
