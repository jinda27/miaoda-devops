package startup

import (
	"context"
	"errors"
	"time"
)

// PreparePortableGit 以工具实际可运行作为继续启动的条件。
func PreparePortableGit(ctx context.Context, initialize, verify func(context.Context) error) (initErr, verifyErr error) {
	if initialize != nil {
		initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		initErr = initialize(initCtx)
		if initCtx.Err() != nil {
			initErr = errors.Join(initErr, initCtx.Err())
		}
		cancel()
	}

	// 首次解包后工具可能暂未就绪，在有限时间内复检。
	verifyCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, stop := context.WithTimeout(verifyCtx, 3*time.Second)
		verifyErr = verify(probeCtx)
		stop()
		if verifyErr == nil {
			return initErr, nil
		}
		select {
		case <-verifyCtx.Done():
			return initErr, verifyErr
		case <-ticker.C:
		}
	}
}
