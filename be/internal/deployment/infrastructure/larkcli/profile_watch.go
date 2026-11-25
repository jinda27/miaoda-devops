package larkcli

import (
	"context"
	"time"
)

const profileWatchInterval = 1500 * time.Millisecond

func (c *Client) watchProfile(ctx context.Context, name string, ready bool) (Profile, error) {
	ticker := time.NewTicker(profileWatchInterval)
	defer ticker.Stop()
	for {
		// 一次只运行一个读取命令，短暂的存储错误在下轮重试。
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		profile, err := c.SelectProfile(probeCtx, name, ready)
		cancel()
		if err == nil {
			return profile, nil
		}
		select {
		case <-ctx.Done():
			return Profile{}, err
		case <-ticker.C:
		}
	}
}

func (c *Client) completeWithProfile(ctx context.Context, name string, ready, observe bool, run func(context.Context) error) (Profile, error) {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var confirmed <-chan Profile
	if observe {
		result := make(chan Profile, 1)
		confirmed = result
		// 监听独立于 CLI 的退出，避免已落盘的状态一直卡在等待页。
		go func() {
			profile, err := c.watchProfile(runCtx, name, ready)
			if err == nil {
				result <- profile
			}
		}()
	}
	finished := make(chan error, 1)
	go func() { finished <- run(runCtx) }()
	var err error
	var observed *Profile
	select {
	case err = <-finished:
	case profile := <-confirmed:
		observed = &profile
		// 给 CLI 留出校验和清理时间，优先保留明确的失败结果。
		grace := time.NewTimer(4 * time.Second)
		defer grace.Stop()
		select {
		case err = <-finished:
		case <-grace.C:
			stop()
			<-finished
			if err := ctx.Err(); err != nil {
				return Profile{}, err
			}
			return profile, nil
		case <-ctx.Done():
			<-finished
			return Profile{}, ctx.Err()
		}
	}
	stop()
	if err != nil {
		return Profile{}, err
	}
	if observed != nil {
		return *observed, nil
	}
	// CLI 退出与配置落盘可能有短暂间隔，继续等待本地状态。
	verifyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.watchProfile(verifyCtx, name, ready)
}
