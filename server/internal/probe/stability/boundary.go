package stability

import "context"

// stageOutcome 一档开环发压的判定结局
type stageOutcome struct {
	limited bool // 本档判定触发限速
	stopped bool // 本档撞全局硬闸提前收尾（样本不完整）
}

// boundary RPM/TPM 速率边界搜索的结论（速率单位由调用方定：req/s 或 token/s）
type boundary struct {
	lo        float64 // 已知可持续的最高速率（完整跑完且未限速）
	hi        float64 // 已知触发限速的最低速率（haveHi 时有效）
	haveHi    bool
	truncated bool // 被全局硬闸截断
}

// searchBoundary 开环速率边界搜索：按 ramp 几何升压到首个限速档，再在 lo/hi 之间二分，
// 档间速率差不大于 tol 或用完 steps 即停。RPM 与 TPM 共用（只是速率单位不同）。
//
// 撞硬闸的那一档样本不完整：它若已判限速，限速证据照样有效（记 hi）；它若「通过」则不可信，
// 不得抬高 lo——否则被截断的下界会被当成收敛值报出去（修复前的 bug）。
func searchBoundary(ctx context.Context, ramp []float64, steps int, tol float64, run func(rate float64) (stageOutcome, error)) (boundary, error) {
	var b boundary
	// try 跑一档并按上述规则更新结论；返回 false = 应停止搜索
	try := func(rate float64) (bool, error) {
		out, err := run(rate)
		if err != nil {
			return false, err
		}
		switch {
		case out.limited:
			b.hi, b.haveHi = rate, true
		case !out.stopped:
			b.lo = rate
		}
		if out.stopped {
			b.truncated = true
		}
		return !out.stopped && ctx.Err() == nil, nil
	}

	for _, rate := range ramp {
		more, err := try(rate)
		if err != nil || !more {
			return b, err
		}
		if b.haveHi {
			break
		}
	}
	for i := 0; b.haveHi && i < steps && b.hi-b.lo > tol; i++ {
		more, err := try((b.lo + b.hi) / 2)
		if err != nil || !more {
			return b, err
		}
	}
	return b, nil
}
