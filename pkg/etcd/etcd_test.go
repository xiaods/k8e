package etcd

import (
	"fmt"
	"testing"
)

func Test_ETCD_getAdvertiseAddress(t *testing.T) {
	fmt.Println(getAdvertiseAddress(""))
}
