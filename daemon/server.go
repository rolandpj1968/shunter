package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
)

func adapterNone() adapter.Set { return adapter.Set{} }

// Serve answers shunt commands on a unix socket until ctx ends. The socket
// is the only way in: it is created with mode 0600, so only the daemon's
// user can command it.
func (d *Daemon) Serve(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("serve: no socket path")
	}
	if len(path) >= 104 {
		return fmt.Errorf("serve: the socket path is %d bytes; unix sockets allow about 100: give a shorter --socket", len(path))
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		// A socket left by a daemon that died. The store's lock, held by
		// now, proves no other daemon owns it.
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/command", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.Exec(ctx, req))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Call sends one command to the daemon on its socket.
func Call(ctx context.Context, sock string, req Request) (Response, error) {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://shunter/v1/command", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := c.Do(hr)
	if err != nil {
		return Response{}, fmt.Errorf("the daemon at %s: %w", sock, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("the daemon answered %s", res.Status)
	}
	var resp Response
	err = json.NewDecoder(res.Body).Decode(&resp)
	return resp, err
}
