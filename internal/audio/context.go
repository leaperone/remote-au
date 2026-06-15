package audio

import (
	"log"

	"github.com/gen2brain/malgo"
)

func initContext(verbose bool) (*malgo.AllocatedContext, error) {
	var logProc malgo.LogProc
	if verbose {
		logProc = func(message string) {
			log.Printf("malgo: %s", message)
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
