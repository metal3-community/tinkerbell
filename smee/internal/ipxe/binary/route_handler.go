package binary

import (
	"context"
	"errors"
	"io"
	"io/fs"

	tftphandler "github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
)

// HandlerRoute adapts a pkg/tftp handler.Handler into a Route so handlers
// written against the generic TFTP mux (eg. the script template generator or
// the OSIE asset server) can participate in the Router's fall-through chain.
//
// A handler result of handler.ErrNotFound or fs.ErrNotExist maps to
// handled=false so the Router continues to the next Route; any other result
// (success or failure) means the handler owned the request.
type HandlerRoute struct {
	RouteName string
	Handler   tftphandler.Handler
}

func (r HandlerRoute) Name() string { return r.RouteName }

func (r HandlerRoute) TryServe(_ context.Context, req Request, w io.ReaderFrom) (bool, error) {
	err := r.Handler.ServeTFTP(req.Filename, w)
	if errors.Is(err, tftphandler.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return true, err
}
