package audio

import (
	"github.com/gen2brain/malgo"

	"remote-au/internal/logging"
)

func initContext(debug bool, logger logging.Logger) (*malgo.AllocatedContext, error) {
	if logger == nil {
		logger = logging.Nop()
	}
	var logProc malgo.LogProc
	if debug {
		logProc = func(message string) {
			logger.Debugf("malgo: %s", message)
		}
	}
	return malgo.InitContext(nil, malgo.ContextConfig{}, logProc)
}

func closeContext(ctx *malgo.AllocatedContext) error {
	if ctx == nil {
		return nil
	}
	err := ctx.Uninit()
	ctx.Free()
	return err
}
