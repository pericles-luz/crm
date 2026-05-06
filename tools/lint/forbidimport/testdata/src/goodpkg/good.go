// Fixture: a normal domain package importing nothing from the forbidden
// list. The analyzer must stay silent here.
package goodpkg

import (
	"context"
	"errors"
	"fmt"
)

func Hello(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("empty")
	}
	_ = fmt.Sprintf("hello %s", name)
	_ = ctx
	return nil
}
