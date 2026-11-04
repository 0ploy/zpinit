//go:build !linux

package vsockexec

import (
	"context"
	"errors"
	"log/slog"

	"github.com/0ploy/zpinit/internal/reaper"
)

// Config is the exec block of machine.json.
type Config struct {
	User       string
	WorkingDir string
	Env        []string
}

// Server is Linux-only; machine mode never triggers elsewhere.
type Server struct{}

// Listener is Linux-only.
type Listener struct{}

func NewServer(Config, *reaper.Reaper, *slog.Logger) *Server { return &Server{} }

func ListenVsock(uint32) (*Listener, error) { return nil, errors.New("vsock is Linux-only") }

func (*Server) Serve(context.Context, *Listener) error { return errors.New("vsock is Linux-only") }
