// Copyright (c) 2017 Gorillalabs. All rights reserved.

package backend

import (
	"io"

	be "github.com/rancher/go-powershell/backend"
)

type Waiter interface {
	Wait() error
}

type Starter interface {
	StartProcess(cmd string, args ...string) (be.Waiter, io.Writer, io.Reader, io.Reader, error)
}
