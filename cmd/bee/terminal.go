package main

import (
	"io"
	"os"
)

type terminalMarker interface {
	IsTerminal() bool
}

func writerIsTerminal(output io.Writer) bool {
	if marker, ok := output.(terminalMarker); ok {
		return marker.IsTerminal()
	}
	file, ok := output.(*os.File)
	return ok && terminalFile(file)
}
