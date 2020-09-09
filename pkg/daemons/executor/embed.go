package executor

import "context"

type Embedded struct{}

func (Embedded) APIServer(ctx context.Context) {

}
