package datadir

import (
	"fmt"
	"testing"
)

func Test_datadir(t *testing.T) {
	datadir, err := LocalHome("", true)
	fmt.Println(datadir, err)
}
